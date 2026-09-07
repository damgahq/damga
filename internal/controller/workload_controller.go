/*
Copyright 2026 Orhan Yavuz.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// WorkloadReconciler renders one Workload into the objects a production
// workload needs, and keeps them that way.
type WorkloadReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ClusterIssuer names the cert-manager ClusterIssuer that signs the
	// certificate for a Workload's domain. Empty means defaultClusterIssuer.
	//
	// A property of the installation and not of the workload, which is why it
	// is here rather than on the spec: a kind cluster issues from the local CA
	// in cluster/issuers.yaml, a public install from Let's Encrypt, and the
	// workload's author has no way to know which and no business choosing. The
	// chart has treated the issuer as a value since the Certificate was written
	// there, and this is the same value one layer down.
	//
	// Nothing sets it yet, so the behaviour is exactly what it was before this
	// field existed. Wiring it is a flag in cmd/operator/main.go and an argument
	// in config/manager, neither of which this change owns.
	ClusterIssuer string
}

// certificatePollInterval is how long to wait before looking again at a
// certificate that has not been issued.
//
// A poll and not a watch, deliberately. Owns(&Certificate{}) would open an
// informer for a kind that does not exist on a cluster without cert-manager,
// and a watch that cannot be established stops the manager from starting — so
// watching would turn "this workload has no certificate yet" into "the operator
// does not run". Nothing else in this reconcile hears about the certificate, so
// without this the status would go on saying Pending long after cert-manager
// had finished.
//
// Thirty seconds is chosen rather than measured: short enough that a
// certificate from a local CA, which is immediate, shows up while somebody is
// still watching the terminal, and long enough that a published workload costs
// two reads a minute rather than sixty.
const certificatePollInterval = 30 * time.Second

// clusterIssuer is the issuer to ask, which is ClusterIssuer unless nothing
// configured one.
func (r *WorkloadReconciler) clusterIssuer() string {
	if r.ClusterIssuer == "" {
		return defaultClusterIssuer
	}
	return r.ClusterIssuer
}

// +kubebuilder:rbac:groups=platform.damga.co,resources=workloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.damga.co,resources=workloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.damga.co,resources=workloads/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var app platformv1alpha1.Workload
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		// Gone. Every object this created carries an owner reference, so the
		// garbage collector has already dealt with them.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	normalise(&app)

	if err := r.reconcileOwned(ctx, &app); err != nil {
		logger.Error(err, "rendering the workload failed")
		if statusErr := r.updateStatus(ctx, &app, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, &app, nil); err != nil {
		return ctrl.Result{}, err
	}

	// Nothing watches the Certificate, so nothing would wake this controller
	// when cert-manager issues one. See certificatePollInterval for why the
	// watch is not there to be had.
	if app.Spec.Domain != "" && !apimeta.IsStatusConditionTrue(app.Status.Conditions, tlsCondition) {
		return ctrl.Result{RequeueAfter: certificatePollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// normalise fills in what the CRD's defaults would have supplied. An Workload
// built in Go and handed straight to the reconciler never passes through the API
// server's defaulting, and a zero Quantity means "no CPU at all" rather than
// "unset" — which the admission policies would rightly reject.
func normalise(app *platformv1alpha1.Workload) {
	app.Spec.Resources.CPURequest = quantityOrDefault(app.Spec.Resources.CPURequest, "100m")
	app.Spec.Resources.MemoryRequest = quantityOrDefault(app.Spec.Resources.MemoryRequest, "128Mi")
	app.Spec.Resources.MemoryLimit = quantityOrDefault(app.Spec.Resources.MemoryLimit, "512Mi")

	if app.Spec.Port == 0 {
		app.Spec.Port = 8080
	}
	if app.Spec.Health.LivenessPath == "" {
		app.Spec.Health.LivenessPath = "/healthz"
	}
	if app.Spec.Health.ReadinessPath == "" {
		app.Spec.Health.ReadinessPath = "/readyz"
	}
}

// reconcileClaims creates the volumes a workload asked for, and never deletes
// one.
//
// Deliberately not owned by the Workload, and this is the same decision the
// database's backup volume already made: an owner reference would delete the
// data the moment somebody deleted the app to recreate it. A claim that
// outlives its workload is a claim somebody can still get their data out of;
// one that does not is a delete key wearing a redeploy's clothes.
//
// A volume removed from the spec therefore leaves its claim behind. That is
// visible in `kubectl get pvc` and reversible; the alternative is not.
func (r *WorkloadReconciler) reconcileClaims(ctx context.Context, app *platformv1alpha1.Workload) error {
	for i := range app.Spec.Volumes {
		want := desiredClaim(app, app.Spec.Volumes[i])
		var have corev1.PersistentVolumeClaim
		err := r.Get(ctx, client.ObjectKeyFromObject(want), &have)
		switch {
		case apierrors.IsNotFound(err):
			if err := r.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("volume %s: %w", app.Spec.Volumes[i].Name, err)
			}
		case err != nil:
			return fmt.Errorf("volume %s: %w", app.Spec.Volumes[i].Name, err)
		}
		// No update pass. Almost every field of a bound claim is immutable, and
		// the one that is not — a size increase — needs a StorageClass that
		// allows expansion and a node that can take it. Attempting it here
		// would fail every reconcile on a class that cannot.
	}
	return nil
}

// reconcileScheduledTasks renders one CronJob per schedule and removes the ones
// a deleted schedule left behind.
//
// A list rather than one object, which is why it cannot use deleteIfPresent:
// that helper looks for an object named after the workload, and these are named
// after the workload and the task. Finding what to remove therefore means
// listing what is there — by the workload's own selector, so the search cannot
// reach another app's — and deleting whatever carries a task label the spec no
// longer names.
//
// Removed and not left, unlike a volume's claim. The asymmetry is deliberate
// and it is about what is lost: a claim holds data that no commit can
// re-create, so it outlives the workload on purpose. A CronJob holds nothing. A
// schedule taken out of the manifest and still firing every night is the
// failure this platform would be judged by, and it is invisible — the app looks
// right, the manifest looks right, and something keeps running.
func (r *WorkloadReconciler) reconcileScheduledTasks(
	ctx context.Context, app *platformv1alpha1.Workload,
) error {
	wanted := make(map[string]bool, len(app.Spec.Schedules))
	for i := range app.Spec.Schedules {
		task := app.Spec.Schedules[i]
		want, err := desiredCronJob(app, task)
		if err != nil {
			// The name is too long to be a CronJob. Reported as a render
			// failure with both halves named, rather than pushed at the API
			// server so it can answer that a name is too long without saying
			// which two were added together.
			return err
		}
		wanted[task.Name] = true

		if err := r.apply(ctx, app, want, func(existing, desired client.Object) {
			e := existing.(*batchv1.CronJob)
			d := desired.(*batchv1.CronJob)
			// The whole spec, including Suspend. A CronJob suspended by hand is
			// reverted on the next pass, and that is the write path working
			// rather than a bug: the manifest carries a suspend field so that
			// pausing a task is a commit somebody can see, and honouring a
			// kubectl patch instead would make the committed state a
			// suggestion.
			e.Spec = d.Spec
			e.Labels = reconcileLabels(e.Labels, d.Labels)
			e.Annotations = reconcileAnnotations(e.Annotations, d.Annotations)
		}); err != nil {
			return fmt.Errorf("scheduled task %s: %w", task.Name, err)
		}
	}

	var have batchv1.CronJobList
	if err := r.List(ctx, &have,
		client.InNamespace(app.Namespace),
		client.MatchingLabels(selectorFor(app)),
	); err != nil {
		return fmt.Errorf("listing scheduled tasks: %w", err)
	}
	for i := range have.Items {
		job := &have.Items[i]
		task := job.Labels[scheduledTaskLabel]
		switch {
		case task == "" || wanted[task]:
			continue
		case !metav1.IsControlledBy(job, app):
			// The same rule apply enforces on the way in. An object this
			// workload does not own is one somebody else made, and a selector
			// that happens to match it is not a licence to delete it.
			log.FromContext(ctx).Info(
				"leaving a CronJob alone because this workload does not own it",
				"name", job.Name)
			continue
		}
		if err := r.Delete(ctx, job); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing scheduled task %s: %w", task, err)
		}
	}
	return nil
}

// reconcileGeneratedSecret mints what the workload asked for, once.
//
// Read before write, and that read is the whole mechanism: desiredGeneratedSecret
// keeps every value it is handed and only invents the missing ones, so a
// reconcile that could not read the existing Secret would mint a fresh set and
// hand the application credentials its own data no longer matches. The read
// failing is therefore an error rather than a nil, unlike the not-found case.
//
// Not owned by the Workload, for the reason its volumes are not: deleting an app
// to recreate it must not destroy the only copy of a password. The Secret
// outlives the workload and is removed with the namespace or by hand.
func (r *WorkloadReconciler) reconcileGeneratedSecret(
	ctx context.Context, app *platformv1alpha1.Workload,
) error {
	if len(app.Spec.Secrets) == 0 {
		return nil
	}
	key := client.ObjectKey{Name: generatedSecretName(app), Namespace: app.Namespace}

	var have corev1.Secret
	err := r.Get(ctx, key, &have)
	switch {
	case apierrors.IsNotFound(err):
		want, err := desiredGeneratedSecret(app, nil)
		if err != nil {
			return err
		}
		if err := r.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("generated secret: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading the generated secret: %w", err)
	}

	want, err := desiredGeneratedSecret(app, &have)
	if err != nil {
		return err
	}
	// Nothing to say. Compared on the key set rather than on the values,
	// because the values are equal by construction — anything already there was
	// copied — and comparing them would mean logging a diff of secrets.
	if len(want.StringData) == len(have.Data) {
		return nil
	}
	have.StringData = want.StringData
	return r.Update(ctx, &have)
}

func (r *WorkloadReconciler) reconcileOwned(ctx context.Context, app *platformv1alpha1.Workload) error {
	// Before the Deployment, which names the claims: a pod referring to a claim
	// that does not exist stays Pending without saying why in the Deployment.
	if err := r.reconcileClaims(ctx, app); err != nil {
		return err
	}

	// Before the Deployment, which mounts it. A pod that names a Secret which
	// does not exist stays Pending, and the Deployment says nothing about why.
	if err := r.reconcileScheduledTasks(ctx, app); err != nil {
		return err
	}

	if err := r.reconcileGeneratedSecret(ctx, app); err != nil {
		return err
	}

	// Order matters only for the ServiceAccount, which the Deployment names.
	if err := r.apply(ctx, app, desiredServiceAccount(app), func(existing, desired client.Object) {
		e := existing.(*corev1.ServiceAccount)
		d := desired.(*corev1.ServiceAccount)
		e.AutomountServiceAccountToken = d.AutomountServiceAccountToken
		e.Labels = reconcileLabels(e.Labels, d.Labels)
	}); err != nil {
		return fmt.Errorf("service account: %w", err)
	}

	if err := r.apply(ctx, app, desiredDeployment(app), func(existing, desired client.Object) {
		e := existing.(*appsv1.Deployment)
		d := desired.(*appsv1.Deployment)
		// Replicas is left alone when autoscaling owns it, so that reconciling
		// does not undo the autoscaler's most recent decision.
		if d.Spec.Replicas != nil {
			e.Spec.Replicas = d.Spec.Replicas
		}
		e.Spec.Selector = d.Spec.Selector
		e.Spec.Strategy = d.Spec.Strategy
		e.Spec.Template = d.Spec.Template
		e.Labels = reconcileLabels(e.Labels, d.Labels)
		// Annotations too, and not only on create. The rollout id changes on
		// every deploy by definition, so an existing Deployment that keeps its
		// first one is an observer permanently attaching new deploys to the
		// oldest record — or, once that record is closed, to nothing.
		//
		// Merged rather than assigned, because this operator is not the only
		// writer here: the deployment controller owns
		// deployment.kubernetes.io/revision on this same map. See
		// reconcileAnnotations for what deleting it costs.
		e.Annotations = reconcileAnnotations(e.Annotations, d.Annotations)
	}); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}

	if err := r.apply(ctx, app, desiredService(app), func(existing, desired client.Object) {
		e := existing.(*corev1.Service)
		d := desired.(*corev1.Service)
		e.Spec.Selector = d.Spec.Selector
		// .spec.type is deliberately not written, so an administrator can move
		// this Service to NodePort or LoadBalancer and keep it. Assigning the
		// ports then undid half of that decision: a rendered port carries
		// nodePort 0, so every pass handed the allocated node port back and the
		// API server issued a different one — the type survived and the number
		// it depends on did not. Preserving a field while overwriting what hangs
		// off it is worse than owning neither.
		e.Spec.Ports = reconcileServicePorts(e.Spec.Ports, d.Spec.Ports)
		e.Labels = reconcileLabels(e.Labels, d.Labels)
	}); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	if err := r.apply(ctx, app, desiredNetworkPolicy(app), func(existing, desired client.Object) {
		e := existing.(*networkingv1.NetworkPolicy)
		d := desired.(*networkingv1.NetworkPolicy)
		e.Spec = d.Spec
		e.Labels = reconcileLabels(e.Labels, d.Labels)
	}); err != nil {
		return fmt.Errorf("network policy: %w", err)
	}

	if err := r.apply(ctx, app, desiredPodDisruptionBudget(app), func(existing, desired client.Object) {
		e := existing.(*policyv1.PodDisruptionBudget)
		d := desired.(*policyv1.PodDisruptionBudget)
		e.Spec.MinAvailable = d.Spec.MinAvailable
		e.Spec.Selector = d.Spec.Selector
		e.Labels = reconcileLabels(e.Labels, d.Labels)
	}); err != nil {
		return fmt.Errorf("pod disruption budget: %w", err)
	}

	// The last two are conditional, so each is either applied or removed. Only
	// creating them would leave an Ingress serving a domain the user deleted.
	if hpa := desiredHPA(app); hpa != nil {
		if err := r.apply(ctx, app, hpa, func(existing, desired client.Object) {
			e := existing.(*autoscalingv2.HorizontalPodAutoscaler)
			d := desired.(*autoscalingv2.HorizontalPodAutoscaler)
			// Field by field rather than the whole spec, because Behavior is
			// only partly ours. This renders the two stabilization windows and
			// nothing else, so the API server defaults the scaling policies and
			// selectPolicy beside them — and assigning the spec deleted those
			// defaults every pass, on an object the autoscaler controller is
			// writing to at the same time. Two controllers rewriting one object
			// is the shape of the outage this operator already had once.
			e.Spec.ScaleTargetRef = d.Spec.ScaleTargetRef
			e.Spec.MinReplicas = d.Spec.MinReplicas
			e.Spec.MaxReplicas = d.Spec.MaxReplicas
			e.Spec.Metrics = d.Spec.Metrics
			e.Spec.Behavior = reconcileHPABehavior(e.Spec.Behavior, d.Spec.Behavior)
			e.Labels = reconcileLabels(e.Labels, d.Labels)
		}); err != nil {
			return fmt.Errorf("autoscaler: %w", err)
		}
	} else if err := r.deleteIfPresent(ctx, app, &autoscalingv2.HorizontalPodAutoscaler{}); err != nil {
		return fmt.Errorf("removing autoscaler: %w", err)
	}

	if ing := desiredIngress(app); ing != nil {
		if err := r.apply(ctx, app, ing, func(existing, desired client.Object) {
			e := existing.(*networkingv1.Ingress)
			d := desired.(*networkingv1.Ingress)
			e.Spec = d.Spec
			e.Labels = reconcileLabels(e.Labels, d.Labels)
			// Merged, not assigned. The two keys rendered here are the
			// only ones this operator has an opinion about; a proxy-body-size,
			// a rate limit or an auth-url that an administrator added is not
			// expressible in the Workload spec, so assigning the map deleted it
			// on the next pass with nothing said. reconcileAnnotations fits
			// unchanged: with no damga.co/ key in the desired map its delete
			// pass is inert and it degrades to a merge.
			e.Annotations = reconcileAnnotations(e.Annotations, d.Annotations)
			// And one key retracted by name, because a merge cannot retract
			// outside its own prefix and this is a key the operator itself used
			// to write. Left in place it would still be asking cert-manager's
			// ingress-shim for a second Certificate over the secret the
			// Certificate rendered below already owns, and the two would take
			// turns writing it. Only this one key: the shim also answers to
			// cert-manager.io/issuer and kubernetes.io/tls-acme, and those are
			// somebody's deliberate act rather than this operator's leftovers.
			delete(e.Annotations, shimAnnotation)
		}); err != nil {
			return fmt.Errorf("ingress: %w", err)
		}
	} else if err := r.deleteIfPresent(ctx, app, &networkingv1.Ingress{}); err != nil {
		return fmt.Errorf("removing ingress: %w", err)
	}

	// After the Ingress, and tolerant of cert-manager not being installed.
	//
	// The Ingress is what serves the domain; the certificate is what makes a
	// browser accept it. A cluster with no cert-manager still runs the
	// workload — the ingress controller answers with its own certificate — so
	// a missing kind is reported on the TLS condition rather than failed here.
	// Failing would put every published workload at Ready=False/RenderFailed,
	// which is where a workload that cannot start also sits, and the two would
	// stop being distinguishable.
	if cert := desiredCertificate(app, r.clusterIssuer()); cert != nil {
		err := r.apply(ctx, app, cert, func(existing, desired client.Object) {
			e := existing.(*unstructured.Unstructured)
			d := desired.(*unstructured.Unstructured)
			mergeSpec(e, d)
			e.SetLabels(reconcileLabels(e.GetLabels(), d.GetLabels()))
		})
		if err != nil && !apimeta.IsNoMatchError(err) {
			return fmt.Errorf("certificate: %w", err)
		}
	} else if err := r.deleteIfPresent(ctx, app, certificateFor(app)); err != nil &&
		!apimeta.IsNoMatchError(err) {
		return fmt.Errorf("removing certificate: %w", err)
	}

	return nil
}

// apply creates the object or brings the existing one back to what it should be.
// mutate copies only the fields this operator owns, so that defaults the API
// server filled in — a Service's clusterIP, a Deployment's revision history —
// are not clobbered on every pass.
func (r *WorkloadReconciler) apply(
	ctx context.Context,
	app *platformv1alpha1.Workload,
	desired client.Object,
	mutate func(existing, desired client.Object),
) error {
	existing, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object %T is not a client.Object", desired)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		// Whether this Workload is allowed to touch what it just found, asked
		// before anything is written. SetControllerReference below is not that
		// check: it refuses an object owned by another controller and adopts an
		// unowned one without a word, which is the whole attack. A tenant who
		// may create Workloads and nothing else names one after an object
		// somebody else made by hand — and this controller rewrites its spec,
		// stamps an owner reference on it, and deletes it along with the
		// Workload. It was demonstrated end to end against an administrator's
		// Service: the selector moved to the attacker's pods, the labels were
		// erased, and deleting the Workload took the Service with it. An Ingress
		// goes the same way, hostname and TLS secret included.
		//
		// So: an object that already exists and is not controlled by this
		// Workload is left exactly as it was found. Not deleted, not adopted,
		// not read from. deleteIfPresent has always done this before removing
		// something; the asymmetry was that the write path never did.
		//
		// A UID is the test for "this came from the server". CreateOrUpdate
		// calls Get into this object first, so a hit carries the stored UID and
		// a miss leaves the locally built copy, which has none — and creation
		// has to keep working.
		if existing.GetUID() != "" && !metav1.IsControlledBy(existing, app) {
			return &conflictError{kind: r.kindOf(existing), name: existing.GetName()}
		}
		mutate(existing, desired)
		return controllerutil.SetControllerReference(app, existing, r.Scheme)
	})
	return err
}

// conflictError says the Workload asked for an object that already exists and
// belongs to somebody else.
//
// It is a distinct type rather than a plain error because the status has to tell
// the two apart: a render that failed is this platform's bug, and a name that
// collided is something only the person who chose the name can resolve. Wrapped
// through reconcileOwned with %w, so errors.As finds it.
type conflictError struct {
	kind string
	name string
}

func (e *conflictError) Error() string {
	// "this resource" rather than "this workload": a Database renders objects
	// through the same check, and an error that names the wrong kind sends the
	// reader looking at the wrong object.
	return fmt.Sprintf(
		"%s %q already exists and is not owned by this resource; "+
			"rename it or remove the object that is in the way",
		e.kind, e.name)
}

// kindOf names an object the way a person would. The Kind on a typed object's
// TypeMeta is empty in practice — the scheme is what knows.
func (r *WorkloadReconciler) kindOf(obj client.Object) string {
	gvk, err := apiutil.GVKForObject(obj, r.Scheme)
	if err != nil {
		return fmt.Sprintf("%T", obj)
	}
	return gvk.Kind
}

// deleteIfPresent removes an object this Workload no longer wants — but only if
// this Workload is the thing that created it.
//
// Deleting by name and namespace alone is enough to destroy something that
// merely shares a name. A Workload called `blog` with no domain would delete an
// unrelated Ingress called `blog` in the same namespace, one that could be
// carrying live traffic and that this operator never created. The owner
// reference is the only thing that distinguishes the two.
func (r *WorkloadReconciler) deleteIfPresent(
	ctx context.Context,
	app *platformv1alpha1.Workload,
	obj client.Object,
) error {
	key := client.ObjectKey{Name: app.Name, Namespace: app.Namespace}
	if err := r.Get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, app) {
		log.FromContext(ctx).Info(
			"leaving an object alone because this workload does not own it",
			"kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", key.Name)
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

func (r *WorkloadReconciler) updateStatus(
	ctx context.Context,
	app *platformv1alpha1.Workload,
	reconcileErr error,
) error {
	var dep appsv1.Deployment
	depErr := r.Get(ctx, client.ObjectKey{Name: app.Name, Namespace: app.Namespace}, &dep)
	if depErr != nil && !apierrors.IsNotFound(depErr) {
		return depErr
	}

	app.Status.ObservedGeneration = app.Generation
	app.Status.Replicas = dep.Status.Replicas
	app.Status.ReadyReplicas = dep.Status.ReadyReplicas

	if app.Spec.Domain != "" {
		app.Status.URL = "https://" + app.Spec.Domain
	} else {
		app.Status.URL = ""
	}

	if app.Spec.Domain == "" {
		// Removed rather than left saying something about a domain that is
		// gone. A stale TLS=True beside an empty URL is a claim about nothing.
		apimeta.RemoveStatusCondition(&app.Status.Conditions, tlsCondition)
	} else {
		cert := certificateFor(app)
		certErr := r.Get(ctx, client.ObjectKeyFromObject(cert), cert)
		tls := certificateCondition(app.Spec.Domain, cert, certErr)
		tls.ObservedGeneration = app.Generation
		tls.LastTransitionTime = metav1.Now()
		setCondition(&app.Status.Conditions, tls)
	}

	ready := metav1.Condition{
		Type:               readyCondition,
		ObservedGeneration: app.Generation,
		LastTransitionTime: metav1.Now(),
	}
	var conflict *conflictError
	switch {
	// Before RenderFailed, because a name collision is not a failure of this
	// platform and saying so would send the wrong person looking. It is the one
	// reason on this list that the tenant can act on themselves, and the message
	// names the object so they can.
	case errors.As(reconcileErr, &conflict):
		ready.Status = metav1.ConditionFalse
		ready.Reason = "Conflict"
		ready.Message = conflict.Error()
	case reconcileErr != nil:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "RenderFailed"
		ready.Message = reconcileErr.Error()
	case apierrors.IsNotFound(depErr):
		ready.Status = metav1.ConditionFalse
		ready.Reason = "AwaitingDeployment"
		ready.Message = "the deployment has not been observed yet"
	case dep.Status.ReadyReplicas == 0:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "NoReadyReplicas"
		ready.Message = "no replica has passed its readiness probe"
	default:
		ready.Status = metav1.ConditionTrue
		ready.Reason = "Serving"
		ready.Message = fmt.Sprintf("%d of %d replicas ready", dep.Status.ReadyReplicas, dep.Status.Replicas)
	}
	meta := &app.Status.Conditions
	setCondition(meta, ready)

	return r.Status().Update(ctx, app)
}

// certificateCondition turns whatever was found where the certificate should be
// into the TLS condition.
//
// A condition of its own rather than a term in Ready, because the two answer
// different questions and folding them together gets one of them wrong either
// way. A workload whose certificate is still pending is serving: its pods are
// up and the Ingress routes to them, over the ingress controller's own
// certificate — reporting Ready=False for that says the same thing as a
// workload that cannot start. And the opposite, Ready=True with nothing said
// about TLS, is a green light next to a URL the browser refuses.
//
// Every reason here names something a different person can act on, which is the
// only thing a status is for: install cert-manager, wait, read what cert-manager
// said, or fix the operator's own access.
func certificateCondition(
	domain string,
	cert *unstructured.Unstructured,
	err error,
) metav1.Condition {
	c := metav1.Condition{Type: tlsCondition, Status: metav1.ConditionFalse}
	switch {
	case apimeta.IsNoMatchError(err):
		c.Reason = reasonCertManagerAbsent
		c.Message = "cert-manager is not installed, so nothing can issue a certificate for " +
			domain + "; the ingress controller answers with its own until it is"
	case apierrors.IsNotFound(err):
		c.Reason = reasonAwaitingCert
		c.Message = "the certificate has not been observed yet"
	case err != nil:
		// Forbidden belongs here rather than on the render path: the workload
		// is running and this is the operator's own installation being wrong.
		c.Reason = reasonUnreadable
		c.Message = err.Error()
	default:
		status, message := certificateReady(cert)
		if status == metav1.ConditionTrue {
			c.Status = metav1.ConditionTrue
			c.Reason = reasonIssued
			c.Message = "a certificate for " + domain + " has been issued"
			break
		}
		c.Reason = reasonPending
		// cert-manager's own message, verbatim, because that is where the
		// diagnosis is: a challenge that cannot be solved, an issuer that does
		// not exist, a rate limit, a name that does not resolve. Any sentence
		// written here instead would say that something is wrong without ever
		// saying what.
		c.Message = message
		if c.Message == "" {
			c.Message = "cert-manager has not issued the certificate yet"
		}
	}
	return c
}

// setCondition keeps LastTransitionTime honest: it only moves when the status
// actually changes, so "how long has this been broken" stays answerable.
func setCondition(conditions *[]metav1.Condition, next metav1.Condition) {
	for i, existing := range *conditions {
		if existing.Type != next.Type {
			continue
		}
		if existing.Status == next.Status {
			next.LastTransitionTime = existing.LastTransitionTime
		}
		(*conditions)[i] = next
		return
	}
	*conditions = append(*conditions, next)
}

func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Workload{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&batchv1.CronJob{}).
		Named("workload").
		Complete(r)
}
