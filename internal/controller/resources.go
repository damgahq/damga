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
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/internal/manifest"
)

// Everything in this block is the platform's decision rather than the user's.
// There is no field on Workload that changes any of it, which is the point:
// a security context that can be switched off is a convention, and conventions
// are what the workload written at 2am does not follow.
const (
	runAsUID  int64 = 1000
	runAsGID  int64 = 1000
	graceSecs int64 = 30

	// The single container in a rendered pod. Named here rather than
	// inline because CI's diagnostics and the hardening assertions both
	// reach for containers[0] by this name.
	containerName = "app"

	// maxCronJobName is what Kubernetes allows a CronJob to be called.
	//
	// 52, and the arithmetic is worth carrying because nothing here can
	// discover it: the CronJob controller appends 11 characters to build each
	// Job's name, and a Job's name may not exceed 63. Kubernetes documents the
	// sum. A CronJob created over the limit is refused by the API server, which
	// is a rejection the operator has to turn into a sentence rather than let
	// arrive as a failed apply nobody reads.
	maxCronJobName = 52

	// scheduledTaskLabel marks the CronJobs this workload owns, so the ones a
	// removed schedule left behind can be found and deleted. The task's name
	// and not the object's, because the object's name carries the workload's
	// too and a label value may not be reconstructed by splitting on a
	// separator that is legal inside both halves.
	scheduledTaskLabel = annotationPrefix + "task"

	// cronBackoffLimit is how many times one tick may retry. None.
	//
	// The default is 6, and it is wrong for this. A scheduled task's retry
	// policy is its schedule: a job that runs hourly and fails is tried again
	// in an hour, by the thing whose whole purpose is to try again. Six
	// immediate retries turn one bad night into six identical failures two
	// minutes apart and six pods to read through — and for the case this was
	// built for, a migration, running it five more times after it exited
	// non-zero is the outcome nobody wants.
	//
	// It is the other half of the rule the exec endpoint states: a non-zero
	// exit is a result. A result is recorded, not retried.
	cronBackoffLimit int32 = 0

	// cronStartingDeadlineSecs bounds how late a missed tick may still run.
	//
	// Set rather than left nil, and the reason is a failure that is silent and
	// permanent: with no deadline the controller counts every missed schedule
	// since the last one that ran, and Kubernetes stops scheduling a CronJob
	// that has missed more than a hundred. A control plane down for a weekend
	// leaves a job that never runs again and reports nothing about why.
	//
	// Five minutes also answers the question the other way. A nightly job that
	// the cluster could not start until six hours later is a job that runs in
	// the middle of the working day, which for the batch this exists to run is
	// worse than skipping.
	cronStartingDeadlineSecs int64 = 300

	// cronJobHistory is how many finished Jobs of each kind to keep.
	//
	// Three of each. The successful default is already three; the failed
	// default is one, and one is the wrong number for the only case anybody
	// opens this to look at — a job that has been failing for three nights is
	// a job whose three nights you want to compare.
	cronJobHistory int32 = 3

	// The port the Service and the Ingress agree to call the application
	// port, and the label whose second writer is Argo CD. Both are named
	// because more than one place has to spell them the same way.
	portName       = "http"
	instanceLabel  = "app.kubernetes.io/instance"
	nameLabel      = "app.kubernetes.io/name"
	managedByLabel = "app.kubernetes.io/managed-by"
	componentLabel = "app.kubernetes.io/component"
	managedByDamga = "damga-platform"

	// The default database and role name a Database gets, and the value
	// the CRD defaults them to. Spelled once so the two cannot drift.
	defaultDatabaseName = "app"

	// Dropped from every container this platform renders.
	capabilityAll = "ALL"

	// Where the tmp volume is mounted. A read-only root filesystem leaves
	// nowhere else to write, and most runtimes write something.
	tmpPath = "/tmp"

	// dataVolume is the conventional name for a workload's single volume and
	// the database's data directory. Shared so the two never drift.
	dataVolume = "data"

	// restartedAtAnnotation carries the moment a restart was asked for. On the
	// pod template on purpose — it is the only value there that a restart is
	// allowed to move.
	restartedAtAnnotation = "damga.co/restarted-at"

	// readyCondition is the one condition every kind here reports. Named once
	// so a page that reads .status.conditions[?(@.type=="Ready")] works against
	// all of them.
	readyCondition = "Ready"

	// runVolume is the writable /run every hardened container needs: a read-only
	// root filesystem leaves nowhere for a socket.
	runVolume = "run"

	// preStop buys the endpoint removal a head start. Without it, a pod is
	// removed from the Service and its process is killed at the same moment,
	// and whichever loses that race drops requests that were already in flight.
	preStopSleepSecs int64 = 5

	// How long a container may take to answer its first probe before the kubelet
	// gives up. Liveness does not run until the startup probe has succeeded, so
	// this is the budget for a slow start — a JVM, a runtime that compiles on
	// boot, a process that runs a migration first.
	startupPeriodSecs    int32 = 5
	startupFailureBudget int32 = 60

	// What a probe does when the workload says nothing. These were literals in
	// desiredDeployment until spec.health could carry them; they are named now
	// because two places have to agree on what "unset" renders as, and a
	// literal in one of them is how they stop agreeing.
	defaultProbePeriodSecs  int32 = 5
	defaultProbeTimeoutSecs int32 = 3
	defaultProbeFailures    int32 = 3

	// composeGroupLabel marks every object one catalogue entry produced, with
	// the entry's name as the value.
	//
	// Written by the converter — compose/convert.go, where it is unexported, so
	// this is a copy rather than a reference and a test fails when the two stop
	// matching. It is what makes "the workloads of one entry" a set this
	// package can name: an entry with a web front end and a worker is two
	// Workloads that have nothing else in common, since every other label here
	// is derived from the object's own name.
	composeGroupLabel = "damga.co/from-compose"

	// The ingress controller lives here. Namespaces carry
	// kubernetes.io/metadata.name automatically, so this needs no cooperation
	// from whoever installed it.
	ingressNamespace = "ingress-nginx"

	// Link-local. The cloud metadata service sits at 169.254.169.254, and
	// reaching it from an workload container turns any request-forgery bug
	// into cloud credentials. Egress is otherwise open, because an workload
	// that cannot make outbound calls is not a platform, it is a sandbox.
	metadataCIDR = "169.254.0.0/16"

	// ingress-nginx reads its annotations as strings, so "true" is a value here
	// rather than a boolean.
	annotationTrue = "true"

	// The certificate for a published workload, as a cert-manager object.
	//
	// Addressed as unstructured rather than through cert-manager's Go module.
	// One object with six fields does not pay for a dependency, and it would
	// buy nothing that matters here: the operator has to keep running on a
	// cluster where this kind does not exist, and a compiled-in type does not
	// make the kind exist.
	certificateAPIVersion = "cert-manager.io/v1"
	certificateKind       = "Certificate"

	// The issuer the operator asks for when the installation has not named one.
	// See WorkloadReconciler.ClusterIssuer for why that is a field.
	defaultClusterIssuer = "letsencrypt-prod"

	// clusterIssuerKind is what issuerRef points at. Cluster-scoped on purpose:
	// a namespaced Issuer would have to exist in every tenant namespace, which
	// means a tenant could replace the authority that signs their own hostname.
	clusterIssuerKind = "ClusterIssuer"

	// The certificate's lifetime and how early it is replaced, carried over
	// from chart/templates/certificate.yaml where both were chart values.
	// Let's Encrypt issues for 90 days and renews at two thirds of the lifetime
	// by default; stating both makes the intent explicit and lets a private CA
	// use a longer one.
	certificateDuration    = "2160h" // 90 days
	certificateRenewBefore = "720h"  // 30 days

	// A renewal mints a new key rather than reusing the old one. The default is
	// the other way round, so this is a decision rather than a restatement.
	certificateKeyRotation = "Always"

	certificateKeyAlgorithm       = "ECDSA"
	certificateKeySize      int64 = 256

	// tlsCondition is where a published workload reports whether its
	// certificate exists yet. Separate from Ready; see certificateCondition.
	tlsCondition = "TLS"

	// The reasons that condition carries. Named rather than written inline
	// because a reason is the part of a status something else keys on — a
	// panel, an alert, a person's shell loop — and one that drifts silently
	// breaks all of them without breaking anything visible here.
	reasonIssued            = "Issued"
	reasonPending           = "Pending"
	reasonAwaitingCert      = "AwaitingCertificate"
	reasonCertManagerAbsent = "CertManagerAbsent"
	reasonUnreadable        = "Unreadable"

	// shimAnnotation asks cert-manager's ingress-shim to create a Certificate
	// of its own for this Ingress. This operator wrote it until it started
	// rendering the Certificate itself, and now retracts it — see
	// desiredIngress and the Ingress mutate in reconcileOwned.
	shimAnnotation = "cert-manager.io/cluster-issuer"
)

func labelsFor(app *platformv1alpha1.Workload) map[string]string {
	out := map[string]string{
		"app.kubernetes.io/name":       app.Name,
		instanceLabel:                  app.Name,
		"app.kubernetes.io/managed-by": "damga-platform",
	}
	// And the entry this came from, when it came from one. It has to be on the
	// pod rather than only on the Workload, because a NetworkPolicy selects
	// pods and the rule below is what needs to find the siblings.
	//
	// This is a pod template field, so adding it rolls every workload that has
	// it once: the deployment controller hashes .spec.template. Once is the
	// whole cost, and only because the value is stable — it is the entry's
	// name, written by the converter at install time and never computed here.
	// A label whose value this function derived would be a rollout on every
	// reconcile, which is the failure that made platform annotations stop being
	// copied onto pods.
	//
	// Not in selectorFor, and that is not a stylistic split: .spec.selector is
	// immutable, so a label added there would make every Deployment that
	// already exists refuse its next update.
	if group := app.Labels[composeGroupLabel]; group != "" {
		out[composeGroupLabel] = group
	}
	return out
}

func selectorFor(app *platformv1alpha1.Workload) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": app.Name,
		instanceLabel:            app.Name,
	}
}

// annotationPrefix is what marks an annotation as this platform's own, and
// therefore as something worth carrying from a Workload onto what it renders.
//
// A prefix rather than a list because the set grows, and a filter rather than
// copying everything because a Workload arrives through git and through Argo
// CD and picks up their annotations on the way. last-applied-configuration on
// a Deployment it does not describe is noise at best; a sync-wave copied onto
// a child object is an instruction meant for the parent.
const annotationPrefix = "damga.co/"

// platformAnnotations returns the Workload's own annotations, and nil rather
// than an empty map when there are none — an empty map is a diff Argo CD
// reports for ever.
func platformAnnotations(app *platformv1alpha1.Workload) map[string]string {
	var out map[string]string
	for k, v := range app.Annotations {
		if !strings.HasPrefix(k, annotationPrefix) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// reconcileAnnotations brings the platform's annotations on a live object up to
// date without claiming the ones it does not own.
//
// Nothing owns a Deployment's annotation map outright, and this operator least
// of all. The deployment controller owns deployment.kubernetes.io/revision and
// writes it on every sync; kubectl owns last-applied-configuration; Argo CD
// owns its tracking id. Assigning the whole map — which is what the mutate used
// to do — claims all of them, so every reconcile deletes the revision
// annotation, the deployment controller puts it back, that write wakes this
// controller through Owns(&appsv1.Deployment{}), and the two rewrite one object
// without pause. The write the deployment controller loses in that fight is its
// own status write, which is how a Deployment reports READY 0/2 while every pod
// is Ready and its own ReplicaSet reports 2.
//
// So the operator claims exactly one namespace of keys, annotationPrefix, and
// reconciles that namespace and nothing else: desired keys are written, keys
// under the prefix that the Workload has stopped carrying are deleted so a
// rollout id can be retracted, and every other key is left as it was found.
//
// It changes nothing when there is nothing to change — no key is deleted that
// was not there, no value is written that is not different, and an object with
// no annotations keeps a nil map rather than growing an empty one. That is what
// makes the reconcile idempotent, and idempotence here is not tidiness: a write
// per pass is what turns a shared object into a war.
func reconcileAnnotations(existing, desired map[string]string) map[string]string {
	for k := range existing {
		if _, wanted := desired[k]; wanted || !strings.HasPrefix(k, annotationPrefix) {
			continue
		}
		delete(existing, k)
	}
	for k, v := range desired {
		if existing == nil {
			existing = make(map[string]string, len(desired))
		}
		existing[k] = v
	}
	return existing
}

// reconcileLabels writes the labels this operator sets and leaves every other
// label on the object alone.
//
// A merge and not an assignment for the same reason as the annotations, with a
// sharper edge: app.kubernetes.io/instance is one of the keys written here, and
// it is also Argo CD's default tracking label. The chart renders the same kinds
// into the same namespace as the operator, so a Workload that shares a name with
// a release puts two writers on one object, each rewriting that label with its
// own idea of what the instance is — and Argo CD reads its own tracking label to
// decide what it owns.
//
// No delete pass, unlike the annotations. labelsFor returns a fixed set, so
// there is never a key this operator wrote and has since stopped wanting;
// adding one that varies would mean this needs the same retraction the
// annotations have. It writes nothing when nothing differs, which is what keeps
// the reconcile from producing an Update on every pass.
func reconcileLabels(existing, desired map[string]string) map[string]string {
	for k, v := range desired {
		if existing == nil {
			existing = make(map[string]string, len(desired))
		}
		existing[k] = v
	}
	return existing
}

// reconcileServicePorts writes the ports this operator renders while keeping the
// node port the API server allocated for each one.
//
// nodePort is not a field this platform has an opinion about — it is not
// reachable from the Workload spec and the rendered value is always zero, which
// means "allocate me one". Writing that zero back over a Service that has one
// releases it and the next allocation is a different number, so a Service an
// administrator moved to NodePort changes its port on every reconcile while
// keeping the type that made it matter.
//
// Matched by port number, which is what a nodePort is attached to. A port this
// operator no longer renders is dropped, because the rendered list is the whole
// truth about which ports exist.
func reconcileServicePorts(existing, desired []corev1.ServicePort) []corev1.ServicePort {
	allocated := make(map[int32]int32, len(existing))
	for _, p := range existing {
		if p.NodePort != 0 {
			allocated[p.Port] = p.NodePort
		}
	}
	out := make([]corev1.ServicePort, 0, len(desired))
	for _, p := range desired {
		if p.NodePort == 0 {
			p.NodePort = allocated[p.Port]
		}
		out = append(out, p)
	}
	return out
}

// reconcileHPABehavior writes the stabilization windows this operator sets and
// leaves the scaling policies beside them alone.
//
// Behavior is a struct this platform only half fills in: it renders a
// StabilizationWindowSeconds for each direction and nothing else, so the API
// server defaults the policies and selectPolicy. Assigning the whole Behavior
// deleted those defaults on every pass, and the server put them straight back —
// on an object the autoscaler controller is also writing to. Nothing had been
// observed to break yet, which is the only reason this reads as tidiness rather
// than as the outage it is one busy cluster away from being.
func reconcileHPABehavior(
	existing, desired *autoscalingv2.HorizontalPodAutoscalerBehavior,
) *autoscalingv2.HorizontalPodAutoscalerBehavior {
	if desired == nil {
		return existing
	}
	if existing == nil {
		return desired
	}
	setWindow := func(e, d *autoscalingv2.HPAScalingRules) *autoscalingv2.HPAScalingRules {
		if d == nil {
			return e
		}
		if e == nil {
			return d
		}
		e.StabilizationWindowSeconds = d.StabilizationWindowSeconds
		return e
	}
	existing.ScaleUp = setWindow(existing.ScaleUp, desired.ScaleUp)
	existing.ScaleDown = setWindow(existing.ScaleDown, desired.ScaleDown)
	return existing
}

// withAnnotations attaches annotations to metadata built by objectMeta,
// leaving the field nil when there are none.
func withAnnotations(meta metav1.ObjectMeta, annotations map[string]string) metav1.ObjectMeta {
	meta.Annotations = annotations
	return meta
}

func objectMeta(app *platformv1alpha1.Workload) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      app.Name,
		Namespace: app.Namespace,
		Labels:    labelsFor(app),
	}
}

// desiredServiceAccount exists so the workload has an identity that is not
// `default`, and so that identity can be denied a token. An workload that
// does not call the Kubernetes API has no reason to carry a credential for it.
func desiredServiceAccount(app *platformv1alpha1.Workload) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta:                   objectMeta(app),
		AutomountServiceAccountToken: ptr.To(false),
	}
}

// desiredDeployment renders the Deployment.
//
// It is the only resource that carries the platform's annotations, and the
// only one that needs to: the observer watches Deployments, reads
// damga.co/rollout off one, and attaches the running state to the evidence
// record that annotation names. Putting them on the Service or the autoscaler
// as well would be annotations nothing reads.
// volumeMounts is the emptyDir every pod gets plus whatever the workload asked
// to keep.
func volumeMounts(app *platformv1alpha1.Workload) []corev1.VolumeMount {
	mounts := make([]corev1.VolumeMount, 0, 1+len(app.Spec.Volumes))
	mounts = append(mounts, corev1.VolumeMount{Name: "tmp", MountPath: tmpPath})
	for _, v := range app.Spec.Volumes {
		mounts = append(mounts, corev1.VolumeMount{Name: v.Name, MountPath: v.Path})
	}
	return mounts
}

// podVolumes names the claims. The claims themselves are created by the
// reconciler and deliberately not owned by the Workload — see reconcileClaims.
func podVolumes(app *platformv1alpha1.Workload) []corev1.Volume {
	vols := make([]corev1.Volume, 0, 1+len(app.Spec.Volumes))
	vols = append(vols, corev1.Volume{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	for _, v := range app.Spec.Volumes {
		vols = append(vols, corev1.Volume{
			Name: v.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claimName(app, v.Name),
				},
			},
		})
	}
	return vols
}

// claimName is the workload's name and the volume's, which is what keeps two
// workloads in one namespace from sharing a claim by naming a volume the same.
func claimName(app *platformv1alpha1.Workload, volume string) string {
	return app.Name + "-" + volume
}

// desiredClaim is one PersistentVolumeClaim for a declared volume.
//
// ReadWriteOnce, which is the only mode a single-node install can actually
// provide and the reason the API refuses autoscale alongside volumes.
func desiredClaim(app *platformv1alpha1.Workload, v platformv1alpha1.Volume) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: claimName(app, v.Name), Namespace: app.Namespace,
			Labels: labelsFor(app),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: v.Size},
			},
		},
	}
	if v.StorageClass != "" {
		claim.Spec.StorageClassName = ptr.To(v.StorageClass)
	}
	return claim
}

// containerEnv is the environment every container this platform renders for an
// app receives, in the order that decides who wins.
//
// One function and not two copies, because the Deployment and the CronJob run
// the same program: a scheduled task that saw a different DATABASE_URL from the
// request handler would be a bug nobody could find from either object alone.
// This was inline in desiredDeployment until the CronJob needed it, and the
// ordering below is the part that must not be re-derived — the comments are the
// original ones and the decisions are unchanged.
func containerEnv(app *platformv1alpha1.Workload) ([]corev1.EnvVar, []corev1.EnvFromSource) {
	env := make([]corev1.EnvVar, 0, len(app.Spec.Env))
	for _, e := range app.Spec.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}

	// The database's Secret first, so a variable the tenant sets explicitly in
	// EnvFrom below wins. Someone naming a database and then overriding
	// DB_HOST is pointing the app somewhere else on purpose, and a platform
	// that silently put its own value last would be overruling them.
	envFrom := make([]corev1.EnvFromSource, 0, len(app.Spec.EnvFrom)+2)
	// Generated values before the database's, and both before the tenant's, so
	// the ordering reads the same all the way down: the further from the
	// platform, the later it wins.
	if len(app.Spec.Secrets) > 0 {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: generatedSecretName(app),
				},
			},
		})
	}
	if app.Spec.Database != "" {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: app.Spec.Database},
			},
		})
	}
	// The values the user gave, after the two the platform produced and before
	// any Secret they named themselves: a person who sets DB_HOST here is
	// overriding the database's, which is the same rule the comment above
	// states, one step further out.
	//
	// Referenced by name and never read. The control plane writes this Secret
	// and the operator does not look inside it, which is why a value the user
	// typed can reach a container without existing anywhere the operator, the
	// commit or this object can show it.
	if len(app.Spec.UserSecrets) > 0 {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: manifest.UserSecretName(app.Name)},
			},
		})
	}
	for _, name := range app.Spec.EnvFrom {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
			},
		})
	}

	return env, envFrom
}

func desiredDeployment(app *platformv1alpha1.Workload) *appsv1.Deployment {
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: path,
					Port: intstr.FromInt32(probePort(app)),
				},
			},
			// Zero means the platform's own answer, so every manifest written
			// before these fields existed renders exactly the bytes it did
			// before — which is what keeps adding them from rolling every pod
			// on the next reconcile.
			PeriodSeconds:    orDefault(app.Spec.Health.IntervalSeconds, defaultProbePeriodSecs),
			TimeoutSeconds:   orDefault(app.Spec.Health.TimeoutSeconds, defaultProbeTimeoutSecs),
			FailureThreshold: orDefault(app.Spec.Health.FailureThreshold, defaultProbeFailures),
		}
	}

	env, envFrom := containerEnv(app)

	replicas := app.Spec.Replicas
	if app.Spec.Autoscale != nil {
		// Owned by the HorizontalPodAutoscaler from here on. Writing a replica
		// count into the Deployment as well would make the two fight, with the
		// operator restoring a number the autoscaler had just corrected.
		replicas = nil
	} else if replicas == nil {
		replicas = ptr.To(int32(2))
	}

	// A workload that keeps data cannot roll. MaxSurge 1 asks the new pod to
	// start while the old one is still running, and both want the same
	// ReadWriteOnce claim — the new pod stays Pending with "Multi-Attach error"
	// until progressDeadlineSeconds gives up. Recreate takes the old pod down
	// first, which costs the downtime the volume already implied and is the only
	// thing that actually completes.
	strategy := appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			// Never fewer than the current count while rolling. Combined with
			// readiness gating this is what makes an upgrade cost zero requests
			// rather than a few.
			MaxUnavailable: ptr.To(intstr.FromInt32(0)),
			MaxSurge:       ptr.To(intstr.FromInt32(1)),
		},
	}
	if len(app.Spec.Volumes) > 0 {
		strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		// One at a time, for the same reason. The API refuses autoscale beside
		// volumes; this covers the fixed count, which it does not police.
		replicas = ptr.To(int32(1))
	}

	return &appsv1.Deployment{
		ObjectMeta: withAnnotations(objectMeta(app), platformAnnotations(app)),
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorFor(app)},
			Strategy: strategy,
			Template: corev1.PodTemplateSpec{
				// Labels, and one annotation that is there only when somebody
				// asked for a restart — never the platform's annotations
				// wholesale.
				//
				// The first attempt at restart carried all of them here and a
				// test refused it with the reason: the deployment controller
				// hashes .spec.template alone, so the rollout id — which
				// changes on every deploy by definition — would roll every pod
				// a second time on every deploy. A restart needs a value that
				// changes when a restart is asked for and at no other time,
				// which is a different field rather than a reused one.
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labelsFor(app),
					Annotations: restartAnnotation(app),
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            app.Name,
					AutomountServiceAccountToken:  ptr.To(false),
					TerminationGracePeriodSeconds: ptr.To(graceSecs),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(runAsUID),
						RunAsGroup:     ptr.To(runAsGID),
						FSGroup:        ptr.To(runAsGID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					// Spread across nodes where possible, but ScheduleAnyway: a
					// single-node cluster should still run the workload, just
					// without the guarantee that draining a node is free.
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
						MaxSkew:           1,
						TopologyKey:       corev1.LabelHostname,
						WhenUnsatisfiable: corev1.ScheduleAnyway,
						LabelSelector:     &metav1.LabelSelector{MatchLabels: selectorFor(app)},
					}},
					Containers: []corev1.Container{{
						Name:  containerName,
						Image: app.Spec.Image,
						Ports: []corev1.ContainerPort{{
							Name:          portName,
							ContainerPort: app.Spec.Port,
						}},
						Env:     env,
						EnvFrom: envFrom,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    app.Spec.Resources.CPURequest,
								corev1.ResourceMemory: app.Spec.Resources.MemoryRequest,
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: app.Spec.Resources.MemoryLimit,
							},
						},
						// Liveness is held back until startup succeeds. Without
						// a startup probe the kubelet begins liveness checks
						// immediately, and a container that needs 30 seconds to
						// answer is killed at 15 — every time, forever, with no
						// field on the spec that could rescue it.
						StartupProbe:   startupProbe(app),
						LivenessProbe:  probe(app.Spec.Health.LivenessPath),
						ReadinessProbe: probe(app.Spec.Health.ReadinessPath),
						Lifecycle: &corev1.Lifecycle{
							// The kubelet's own sleep, not /bin/sleep. An exec
							// hook needs that binary to exist in the image, and
							// the images worth running here are distroless ones
							// that have no shell and no coreutils — so the exec
							// form fails silently and the grace period this is
							// supposed to buy never happens.
							PreStop: &corev1.LifecycleHandler{
								Sleep: &corev1.SleepAction{Seconds: preStopSleepSecs},
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						// A read-only root filesystem breaks anything that writes
						// a temp file, which is most runtimes. This gives them
						// somewhere to write that does not survive the pod.
						VolumeMounts: volumeMounts(app),
					}},
					Volumes: podVolumes(app),
				},
			},
		},
	}
}

// cronJobName is what one scheduled task's CronJob is called, or a refusal
// naming both halves.
//
// The workload's name and the task's, joined. Both are DNS labels and either
// may be long: a workload's name may be a full 63 characters, so a task name
// this platform bounded at 20 is not on its own enough to keep the sum inside
// what Kubernetes allows. Checked here rather than left to the API server,
// because the API server's refusal arrives as a failed apply on an object
// nobody is looking at, and what it says is that a name is too long — not
// which two names were added together to make it.
func cronJobName(app *platformv1alpha1.Workload, task string) (string, error) {
	name := app.Name + "-" + task
	if len(name) > maxCronJobName {
		return "", fmt.Errorf(
			"the CronJob for task %q would be called %q, which is %d characters; "+
				"Kubernetes allows a CronJob %d, because it appends 11 to name each Job. "+
				"Shorten the task name or the workload's",
			task, name, len(name), maxCronJobName)
	}
	return name, nil
}

// desiredCronJob renders one scheduled task.
//
// The application's own image, its environment and its identity — the same
// container the Deployment runs, with a command instead of a server. That is
// the whole point of holding schedules on the workload: the nightly job and the
// request handler are the same build, and containerEnv is shared so they cannot
// disagree about what the database is called.
//
// What it deliberately does not carry from the Deployment: probes, ports, the
// topology spread and the preStop sleep. A batch container answers no HTTP, is
// not in a Service, and has nothing to drain — a readiness probe on it would
// fail for ever and a liveness probe would kill a long migration at fifteen
// seconds.
func desiredCronJob(
	app *platformv1alpha1.Workload, task platformv1alpha1.ScheduledTask,
) (*batchv1.CronJob, error) {
	name, err := cronJobName(app, task.Name)
	if err != nil {
		return nil, err
	}
	env, envFrom := containerEnv(app)

	labels := labelsFor(app)
	labels[scheduledTaskLabel] = task.Name

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   app.Namespace,
			Labels:      labels,
			Annotations: platformAnnotations(app),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: task.Schedule,
			Suspend:  ptr.To(task.Suspend),
			// Forbid, where Kubernetes defaults to Allow. The named case is a
			// batch over the application's own data, and two copies of it
			// writing the same rows is the failure a schedule invites: a run
			// that takes longer than its interval overlaps itself, for ever,
			// and each one makes the next one slower.
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			StartingDeadlineSeconds:    ptr.To(cronStartingDeadlineSecs),
			SuccessfulJobsHistoryLimit: ptr.To(cronJobHistory),
			FailedJobsHistoryLimit:     ptr.To(cronJobHistory),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					BackoffLimit: ptr.To(cronBackoffLimit),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							// Never, not OnFailure. With BackoffLimit at zero
							// the two differ in what is left behind: OnFailure
							// restarts the container in the same pod and the
							// previous attempt's log goes with it, and the
							// whole reason to keep failed Jobs is to read it.
							RestartPolicy:                corev1.RestartPolicyNever,
							ServiceAccountName:           app.Name,
							AutomountServiceAccountToken: ptr.To(false),
							SecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot:   ptr.To(true),
								RunAsUser:      ptr.To(runAsUID),
								RunAsGroup:     ptr.To(runAsGID),
								FSGroup:        ptr.To(runAsGID),
								SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
							},
							Containers: []corev1.Container{{
								Name:    containerName,
								Image:   app.Spec.Image,
								Command: task.Command,
								Env:     env,
								EnvFrom: envFrom,
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    app.Spec.Resources.CPURequest,
										corev1.ResourceMemory: app.Spec.Resources.MemoryRequest,
									},
									Limits: corev1.ResourceList{
										corev1.ResourceMemory: app.Spec.Resources.MemoryLimit,
									},
								},
								SecurityContext: &corev1.SecurityContext{
									AllowPrivilegeEscalation: ptr.To(false),
									ReadOnlyRootFilesystem:   ptr.To(true),
									Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
								},
								// The same mounts the Deployment gets,
								// including the emptyDir at /tmp that a
								// read-only root filesystem needs. A batch that
								// writes a temp file is the ordinary case.
								VolumeMounts: volumeMounts(app),
							}},
							Volumes: podVolumes(app),
						},
					},
				},
			},
		},
	}, nil
}

// startupProbe gives a slow container startupFailureBudget * startupPeriodSecs
// seconds to answer once, and asks nothing of it after that.
func startupProbe(app *platformv1alpha1.Workload) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: app.Spec.Health.LivenessPath,
				Port: intstr.FromInt32(probePort(app)),
			},
		},
		// The budget stays the platform's on purpose. spec.health.intervalSeconds
		// is how often a *running* container is asked whether it is still alive;
		// this is how long a starting one is given to answer at all, and a
		// workload that lengthened its interval would otherwise shorten nothing
		// and lengthen its startup budget by twelve times without asking.
		PeriodSeconds:    startupPeriodSecs,
		TimeoutSeconds:   orDefault(app.Spec.Health.TimeoutSeconds, defaultProbeTimeoutSecs),
		FailureThreshold: startupFailureBudget,
	}
}

// probePort is where the probes knock.
//
// spec.health.port when it is set, and the workload's own port when it is not.
// The second is the assumption that was here unconditionally, and it is the
// most common reason a catalogue entry never becomes Ready: an application that
// answers its API on one port and nothing at all on 8080 is probed on 8080,
// fails, and is restarted for ever by the liveness probe.
func probePort(app *platformv1alpha1.Workload) int32 {
	if app.Spec.Health.Port != 0 {
		return app.Spec.Health.Port
	}
	return app.Spec.Port
}

// orDefault is "zero means the platform decides", in one place.
func orDefault(v, fallback int32) int32 {
	if v == 0 {
		return fallback
	}
	return v
}

func desiredService(app *platformv1alpha1.Workload) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: objectMeta(app),
		Spec: corev1.ServiceSpec{
			Selector: selectorFor(app),
			Ports: []corev1.ServicePort{{
				Name:       portName,
				Port:       80,
				TargetPort: intstr.FromString(portName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// desiredNetworkPolicy denies everything this workload is not explicitly
// allowed. One policy is enough: selecting a pod with both policy types set
// means only the listed traffic survives.
func desiredNetworkPolicy(app *platformv1alpha1.Workload) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)
	appPort := intstr.FromInt32(app.Spec.Port)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: objectMeta(app),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selectorFor(app)},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: eastWest(app, networkingv1.NetworkPolicyIngressRule{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": ingressNamespace},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &appPort}},
			}),
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// Forgetting this rule is the most common way a default-deny
					// policy silently breaks an workload: name resolution
					// fails and every outbound call times out with no obvious
					// cause.
					//
					// Addressed to the cluster's resolver and nowhere else. With
					// no "to" this said port 53 to any host on the internet,
					// which is a data exfiltration channel wearing the one
					// protocol nobody blocks: a compromised workload encodes
					// what it stole into names and reads the answers back. The
					// chart's own policy has always scoped it this way; the
					// operator's copy of the rule lost the destination.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k8s-app": "kube-dns"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
				{
					// Everything else, minus the cloud metadata service.
					//
					// This rule is IPv4 only, and that is load-bearing rather
					// than an oversight: an ipBlock matches one family, so with
					// no ::/0 rule beside it egress over IPv6 is denied outright
					// — including to the IPv6 metadata endpoints that this
					// except list does not name. Deny-by-omission is doing real
					// work here, and it is one line away from being undone.
					//
					// So whoever makes this platform dual-stack: adding a ::/0
					// egress rule without an Except carrying the IPv6 metadata
					// ranges (fd00:ec2::254/128 on AWS, and fe80::/10 for
					// link-local generally) reopens SSRF-to-cloud-credentials on
					// the day it lands, with nothing in the diff to say so.
					// TestEgressHasNoIPv6Hole fails if that rule appears without
					// them.
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{
							CIDR:   "0.0.0.0/0",
							Except: []string{metadataCIDR},
						},
					}},
				},
			},
		},
	}
}

// eastWest lets the workloads of one catalogue entry reach each other, and
// nothing else reach them.
//
// The gap it closes was measured on a real cluster: a probe carrying a
// workload's own selector labels resolved DNS, reached https://example.com with
// a 200 and ghcr.io with a 401 — a refusal, so the connection was made — and got
// 000 from a sibling Service. Egress was never the problem. What was closed was
// the destination's ingress, which admitted the ingress controller and nobody
// else, so a catalogue entry with a front end and a worker installed cleanly and
// could not talk to itself. Sixty of the 202 entries that install are more than
// one workload.
//
// # Why this does not open the fence
//
// The peer is a bare podSelector with no namespaceSelector beside it, and in
// NetworkPolicy that means "pods in the same namespace as this policy" — it is
// not a wildcard, and no selector in another namespace can satisfy it. A tenant
// lives in its own namespace, so a tenant cannot label its way into another
// tenant's application no matter what it writes on its pods. Inside one
// namespace the value still has to match, so two entries installed side by side
// stay separated from each other.
//
// A namespaceSelector here would be the opposite: two tenants that both install
// n8n would carry the same value, and the rule would join them. That is the
// version of this change that removes the fence rather than opening a door in
// it, and it is why the peer is written this way rather than as the more
// obvious "same label anywhere".
//
// # Why no port list
//
// A Workload declares the one port its Service publishes, and a sibling reaches
// whatever the process listens on — a worker's health port, a queue, a second
// listener the template never named. There is no list to write, and inventing
// one would refuse exactly the traffic this exists to permit. What bounds the
// rule is the peer rather than the port: one entry, one namespace.
//
// A workload with no entry label — anything not installed from the catalogue —
// gets the policy it always had.
func eastWest(
	app *platformv1alpha1.Workload, base networkingv1.NetworkPolicyIngressRule,
) []networkingv1.NetworkPolicyIngressRule {
	rules := make([]networkingv1.NetworkPolicyIngressRule, 0, 2)
	rules = append(rules, base)
	group := app.Labels[composeGroupLabel]
	if group == "" {
		return rules
	}
	return append(rules, networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{composeGroupLabel: group},
			},
		}},
	})
}

func desiredPodDisruptionBudget(app *platformv1alpha1.Workload) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: objectMeta(app),
		Spec: policyv1.PodDisruptionBudgetSpec{
			// Expressed as a percentage rather than a count so it stays correct
			// while the autoscaler moves the replica count around. A fixed
			// minAvailable can silently block every eviction once the app scales
			// down to it.
			MinAvailable: ptr.To(intstr.FromString("50%")),
			Selector:     &metav1.LabelSelector{MatchLabels: selectorFor(app)},
		},
	}
}

func desiredHPA(app *platformv1alpha1.Workload) *autoscalingv2.HorizontalPodAutoscaler {
	if app.Spec.Autoscale == nil {
		return nil
	}
	a := app.Spec.Autoscale

	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: objectMeta(app),
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       app.Name,
			},
			MinReplicas: ptr.To(a.MinReplicas),
			MaxReplicas: a.MaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptr.To(a.TargetCPUPercent),
					},
				},
			}},
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(0)),
				},
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: ptr.To(int32(300)),
				},
			},
		},
	}
}

func desiredIngress(app *platformv1alpha1.Workload) *networkingv1.Ingress {
	if app.Spec.Domain == "" {
		return nil
	}
	pathType := networkingv1.PathTypePrefix
	meta := objectMeta(app)
	// No cert-manager annotation. It used to be here, and asking for the
	// certificate that way is what this change stopped doing: the annotation
	// makes cert-manager's ingress-shim create and own a Certificate of its
	// own, and the object it creates is then tied to the life of one Ingress.
	// Measured, on the chart, before the operator existed — two Ingresses over
	// one hostname and one secret, the second one refused:
	//
	//	certificate resource is not owned by this object.
	//	refusing to update non-owned certificate resource
	//
	// and when the owning Ingress was deleted the Certificate was collected
	// with it and the other one quietly lost its certificate. So the Certificate
	// is rendered explicitly beside this Ingress — see desiredCertificate — and
	// the two are joined by the secret name below and nothing else.
	meta.Annotations = map[string]string{
		"nginx.ingress.kubernetes.io/ssl-redirect":       annotationTrue,
		"nginx.ingress.kubernetes.io/force-ssl-redirect": annotationTrue,
	}

	return &networkingv1.Ingress{
		ObjectMeta: meta,
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("nginx"),
			TLS: []networkingv1.IngressTLS{{
				Hosts:      []string{app.Spec.Domain},
				SecretName: certificateSecretName(app),
			}},
			Rules: []networkingv1.IngressRule{{
				Host: app.Spec.Domain,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: app.Name,
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
}

// certificateSecretName is the Secret the certificate is written to and the
// Secret the Ingress reads. Spelled once, because the two are only a working
// certificate for as long as they agree.
func certificateSecretName(app *platformv1alpha1.Workload) string {
	return app.Name + "-tls"
}

// certificateFor is an empty Certificate carrying nothing but its identity, for
// the paths that read one or delete one rather than render it.
func certificateFor(app *platformv1alpha1.Workload) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetAPIVersion(certificateAPIVersion)
	cert.SetKind(certificateKind)
	cert.SetName(app.Name)
	cert.SetNamespace(app.Namespace)
	return cert
}

// desiredCertificate renders the cert-manager Certificate for a published
// workload, and nothing at all for one that has no domain.
//
// This is chart/templates/certificate.yaml, which had been a working
// specification for the platform's own release since before the operator
// existed, applied to a tenant's workload: same issuer reference, same key
// algorithm, same rotation policy, same renewal window.
//
// Named after the workload, like every other object rendered here, and not
// after the secret it produces. The name is what apply's ownership check and
// deleteIfPresent both key on, and an object named differently from its
// siblings is one those two would have to be taught about separately.
func desiredCertificate(app *platformv1alpha1.Workload, issuer string) *unstructured.Unstructured {
	if app.Spec.Domain == "" {
		return nil
	}
	cert := certificateFor(app)
	cert.SetLabels(labelsFor(app))
	cert.Object["spec"] = map[string]any{
		"secretName": certificateSecretName(app),
		"dnsNames":   []any{app.Spec.Domain},
		"issuerRef": map[string]any{
			"name": issuer,
			"kind": clusterIssuerKind,
		},
		"privateKey": map[string]any{
			"algorithm":      certificateKeyAlgorithm,
			"size":           certificateKeySize,
			"rotationPolicy": certificateKeyRotation,
		},
		"duration":    certificateDuration,
		"renewBefore": certificateRenewBefore,
	}
	return cert
}

// mergeSpec writes the fields this operator renders into a foreign object's
// spec and leaves everything else in there as it was found.
//
// The same rule the labels and the HPA's behaviour are already reconciled
// under, applied to a kind whose schema this repository does not have: a
// cert-manager release can add a field, its webhook can default one, and an
// administrator can set one this operator has no field for. Assigning the whole
// spec deletes all three on the next pass, and the object has another
// controller writing to it.
//
// No delete pass, for the same reason reconcileLabels has none: what
// desiredCertificate renders is a fixed set of keys, so there is never one the
// operator wrote and has since stopped wanting. A key that starts varying needs
// the retraction the annotations have.
func mergeSpec(existing, desired *unstructured.Unstructured) {
	want, ok := desired.Object["spec"].(map[string]any)
	if !ok {
		return
	}
	spec, ok := existing.Object["spec"].(map[string]any)
	if !ok {
		spec = make(map[string]any, len(want))
	}
	maps.Copy(spec, want)
	existing.Object["spec"] = spec
}

// certificateReady reads cert-manager's own verdict off a Certificate.
//
// Unknown covers both "no status yet" and "a status shaped in a way this does
// not recognise", which are the same thing to a caller: nobody has said the
// certificate is issued.
func certificateReady(cert *unstructured.Unstructured) (metav1.ConditionStatus, string) {
	conditions, found, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
	if err != nil || !found {
		return metav1.ConditionUnknown, ""
	}
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := c["type"].(string); t != readyCondition {
			continue
		}
		status, _ := c["status"].(string)
		message, _ := c["message"].(string)
		return metav1.ConditionStatus(status), message
	}
	return metav1.ConditionUnknown, ""
}

// quantityOrDefault keeps a zero Quantity from reaching the API server as "0",
// which would be a request for no CPU rather than the default the CRD declares.
func quantityOrDefault(q resource.Quantity, fallback string) resource.Quantity {
	if q.IsZero() {
		return resource.MustParse(fallback)
	}
	return q
}

// restartAnnotation is the one thing on the pod template that is allowed to
// change without the image changing.
//
// nil rather than an empty map when nothing asked for a restart: a Workload with
// nothing of ours on it must not grow an empty annotation map, which shows up as
// a diff Argo CD reports for ever.
func restartAnnotation(app *platformv1alpha1.Workload) map[string]string {
	if app.Spec.RestartedAt == "" {
		return nil
	}
	return map[string]string{restartedAtAnnotation: app.Spec.RestartedAt}
}

// generatedSecretName is where a workload's minted values live. Its own object
// rather than a key on something the tenant edits, so that reading it back is
// the only way a value survives — and so that deleting the Workload can leave it
// behind, the way a volume's claim is left behind.
func generatedSecretName(app *platformv1alpha1.Workload) string {
	return app.Name + "-generated"
}

// desiredGeneratedSecret mints what the workload asked for, and keeps what it
// already minted.
//
// existing is what the cluster holds, or nil. A caller that cannot read it must
// pass nil rather than an empty Secret — the same contract as
// desiredDatabaseSecret, and for the same reason: every reconcile would
// otherwise mint new values and every pod would restart with credentials the
// application's own data no longer matches.
//
// A value is kept if it is there and regenerated only if it is missing, so
// adding a name to the spec mints one value rather than rotating all of them.
// Removing a name leaves its value in the Secret rather than deleting it,
// because a key that vanishes on an edit is a key an application loses without
// anything saying so; the Secret is cleaned up with the workload or by hand.
func desiredGeneratedSecret(
	app *platformv1alpha1.Workload, existing *corev1.Secret,
) (*corev1.Secret, error) {
	// Guarded, and the guard is the point: the doc comment above says nil means
	// "there is nothing yet", and the first version of this called DeepCopy on
	// it anyway. A nil *Secret deep-copies to nil and reading .Data off that
	// panics — so the contract was written correctly and then not honoured four
	// lines later, which envtest caught on the first run.
	data := map[string]string{}
	if existing != nil {
		for k, v := range existing.Data {
			data[k] = string(v)
		}
	}

	for _, want := range app.Spec.Secrets {
		if data[want.Name] != "" {
			continue
		}
		raw := make([]byte, passwordBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generating %s: %w", want.Name, err)
		}
		switch want.Kind {
		case platformv1alpha1.GeneratedUser:
			// Short and lowercase, with a letter first. Long enough not to
			// collide within one namespace and short enough for the identifier
			// limits a database imposes — PostgreSQL truncates a role at 63
			// bytes and says nothing.
			//
			// Its own alphabet rather than base64: a role name with a hyphen in
			// it has to be quoted in every statement that mentions it, and
			// somewhere down the line one of them will not be.
			data[want.Name] = "u" + strings.ToLower(
				base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))[:11]
		case platformv1alpha1.GeneratedHex:
			data[want.Name] = hex.EncodeToString(raw)
		case platformv1alpha1.GeneratedBase64:
			data[want.Name] = base64.StdEncoding.EncodeToString(raw)
		default:
			// Base64 rather than hex: the same entropy in fewer characters, and
			// every byte of it is safe in a connection string and in a shell.
			data[want.Name] = base64.RawURLEncoding.EncodeToString(raw)
		}
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: generatedSecretName(app), Namespace: app.Namespace,
			Labels: labelsFor(app),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}, nil
}
