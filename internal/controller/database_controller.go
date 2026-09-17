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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// DatabaseReconciler renders one Database into the server a small team gets
// without operating one.
type DatabaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.damga.co,resources=databases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.damga.co,resources=databases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// The pods a backup leaves behind carry the rehearsal's answer in their
// terminated state, and read-only is all that takes.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var db platformv1alpha1.Database
	if err := r.Get(ctx, req.NamespacedName, &db); err != nil {
		// Gone. The StatefulSet and Service carry owner references and the
		// garbage collector removes them — but not the PersistentVolumeClaims a
		// StatefulSet created, which Kubernetes deliberately leaves behind. That
		// is the behaviour this platform wants: deleting the object that
		// describes a database should not be the way its data is destroyed.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	normaliseDatabase(&db)

	if err := r.reconcileDatabase(ctx, &db); err != nil {
		if statusErr := r.updateDatabaseStatus(ctx, &db, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.updateDatabaseStatus(ctx, &db, nil)
}

// normaliseDatabase fills in what the CRD's defaults would have supplied, for
// an object built in Go that never passed through the API server.
func normaliseDatabase(db *platformv1alpha1.Database) {
	// The engine first, because everything below and every name this renders
	// depends on it. Empty has to mean postgres in exactly the way the CRD's
	// default does, or a Database built in Go is labelled differently from the
	// same Database created through the API — and that label is inside a
	// selector no update can change afterwards.
	if db.Spec.Engine == "" {
		db.Spec.Engine = platformv1alpha1.EnginePostgres
	}
	if db.Spec.Database == "" {
		db.Spec.Database = defaultDatabaseName
	}
	if db.Spec.Username == "" {
		db.Spec.Username = defaultDatabaseName
	}
	db.Spec.Resources.CPURequest = quantityOrDefault(db.Spec.Resources.CPURequest, "100m")
	db.Spec.Resources.MemoryRequest = quantityOrDefault(db.Spec.Resources.MemoryRequest, "256Mi")
	db.Spec.Resources.MemoryLimit = quantityOrDefault(db.Spec.Resources.MemoryLimit, "512Mi")

	// The backup's own defaults, for the same reason: an object built in Go
	// never passes through the API server, and an empty schedule renders a
	// CronJob the API server then rejects — which reads as the controller being
	// broken rather than as a field nobody filled in.
	if db.Spec.Backup != nil {
		if db.Spec.Backup.Schedule == "" {
			db.Spec.Backup.Schedule = "0 2 * * *"
		}
		if db.Spec.Backup.RetainDays == 0 {
			db.Spec.Backup.RetainDays = 7
		}
	}
}

func (r *DatabaseReconciler) reconcileDatabase(ctx context.Context, db *platformv1alpha1.Database) error {
	// The Secret first, and read before it is written. A password minted on
	// every pass would lock the application out of a database whose data
	// directory still holds the first one — see desiredDatabaseSecret.
	var live corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Name: db.Name, Namespace: db.Namespace}, &live)
	switch {
	case apierrors.IsNotFound(err):
		// Nothing there. desiredDatabaseSecret generates one.
	case err != nil:
		// Deliberately fatal rather than proceeding with nil. Not being able to
		// read the Secret and generating a fresh password are indistinguishable
		// to everything downstream, and one of them destroys access to a
		// running database.
		return fmt.Errorf("reading the existing credentials: %w", err)
	}
	existing := &live
	if apierrors.IsNotFound(err) {
		existing = nil
	}

	secret, err := desiredDatabaseSecret(db, existing)
	if err != nil {
		return err
	}
	if err := r.applyDatabase(ctx, db, secret, func(e, d client.Object) {
		es, ds := e.(*corev1.Secret), d.(*corev1.Secret)
		es.StringData = ds.StringData
		es.Labels = reconcileLabels(es.Labels, ds.Labels)
	}); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}

	if err := r.applyDatabase(ctx, db, desiredDatabaseService(db), func(e, d client.Object) {
		es, ds := e.(*corev1.Service), d.(*corev1.Service)
		es.Spec.Selector = ds.Spec.Selector
		es.Spec.Ports = reconcileServicePorts(es.Spec.Ports, ds.Spec.Ports)
		es.Labels = reconcileLabels(es.Labels, ds.Labels)
	}); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	if err := r.applyDatabase(ctx, db, desiredDatabaseStatefulSet(db), func(e, d client.Object) {
		es, ds := e.(*appsv1.StatefulSet), d.(*appsv1.StatefulSet)
		es.Spec.Replicas = ds.Spec.Replicas
		es.Spec.Template = ds.Spec.Template
		es.Labels = reconcileLabels(es.Labels, ds.Labels)
		// Selector, ServiceName and VolumeClaimTemplates are deliberately not
		// written on an existing object. All three are immutable on a
		// StatefulSet, so assigning them turns a harmless no-op into a rejected
		// update on every pass — and the one that would matter, a volume grown
		// in the spec, has to be resized on the PersistentVolumeClaim itself.
	}); err != nil {
		return fmt.Errorf("statefulset: %w", err)
	}

	// Backups are conditional, so both directions are handled. Only ever
	// creating them would leave a schedule running against a database whose
	// owner turned backups off — and the archives it keeps writing are the
	// thing they asked to stop paying for.
	//
	// Redis takes the same branch as "no backup asked for", and the API refuses
	// the combination outright so this is only reached by an object built in Go.
	// It is here anyway rather than left to the API: what the other branch
	// renders is a CronJob that runs pg_dump against a server that has never
	// heard of it, and the failure would arrive nightly, in a Job, addressed to
	// nobody.
	if db.Spec.Backup == nil || db.Spec.Engine == platformv1alpha1.EngineRedis {
		if err := r.deleteDatabaseChild(ctx, db, &batchv1.CronJob{}, backupName(db)); err != nil {
			return fmt.Errorf("removing the backup schedule: %w", err)
		}
		// The claim is deliberately left. Deleting a schedule is not asking for
		// the archives to be destroyed, and this platform does not delete data
		// as a side effect of a spec change.
		return nil
	}

	// Created if absent and never owned, which is the opposite of everything
	// else here and is the point.
	//
	// An owner reference would have the garbage collector delete the archives
	// when the Database is deleted — the same cascade that made this a separate
	// kind in the first place, reappearing one level down. A StatefulSet's data
	// volume survives deletion because Kubernetes deliberately leaves
	// volumeClaimTemplates behind; this claim is created directly, so nothing
	// would leave it behind unless it is unowned.
	//
	// The consequence is that a claim already sitting there is used as it is. In
	// a tenant's own namespace that is what somebody recreating a Database wants
	// — their archives, still there — and it is the reason the namespace
	// boundary is doing real work rather than decorating this.
	claim := desiredBackupClaim(db)
	err = r.Get(ctx, client.ObjectKeyFromObject(claim), &corev1.PersistentVolumeClaim{})
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, claim); err != nil {
			return fmt.Errorf("backup volume: %w", err)
		}
	case err != nil:
		return fmt.Errorf("backup volume: %w", err)
	}

	if err := r.applyDatabase(ctx, db, desiredBackupCronJob(db), func(e, d client.Object) {
		ec, dc := e.(*batchv1.CronJob), d.(*batchv1.CronJob)
		ec.Spec = dc.Spec
		ec.Labels = reconcileLabels(ec.Labels, dc.Labels)
	}); err != nil {
		return fmt.Errorf("backup schedule: %w", err)
	}
	return nil
}

// deleteDatabaseChild removes an object this Database made, and only one it
// made. The ownership check is the same question applyDatabase asks: deleting
// by name alone would destroy something that merely shares a name.
func (r *DatabaseReconciler) deleteDatabaseChild(
	ctx context.Context, db *platformv1alpha1.Database, obj client.Object, name string,
) error {
	key := client.ObjectKey{Name: name, Namespace: db.Namespace}
	if err := r.Get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, db) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

// applyDatabase is the Workload reconciler's apply, with the same ownership
// question asked first.
//
// The check is not optional here for the reason it was not there: an object
// that already exists and belongs to somebody else is left alone. A Database
// named after an existing StatefulSet would otherwise adopt it, rewrite its
// pod template, and take it down when the Database was deleted.
func (r *DatabaseReconciler) applyDatabase(
	ctx context.Context,
	db *platformv1alpha1.Database,
	desired client.Object,
	mutate func(existing, desired client.Object),
) error {
	existing, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object %T is not a client.Object", desired)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		if existing.GetUID() != "" && !metav1.IsControlledBy(existing, db) {
			return &conflictError{kind: r.kindOf(existing), name: existing.GetName()}
		}
		mutate(existing, desired)
		return controllerutil.SetControllerReference(db, existing, r.Scheme)
	})
	return err
}

func (r *DatabaseReconciler) kindOf(obj client.Object) string {
	return (&WorkloadReconciler{Scheme: r.Scheme}).kindOf(obj)
}

func (r *DatabaseReconciler) updateDatabaseStatus(
	ctx context.Context, db *platformv1alpha1.Database, reconcileErr error,
) error {
	var set appsv1.StatefulSet
	setErr := r.Get(ctx, client.ObjectKey{Name: db.Name, Namespace: db.Namespace}, &set)
	if setErr != nil && !apierrors.IsNotFound(setErr) {
		return setErr
	}

	db.Status.ObservedGeneration = db.Generation
	db.Status.SecretName = db.Name
	db.Status.Host = databaseHost(db)

	// Kept rather than cleared when the read fails or finds nothing. The last
	// rehearsal happened whether or not this pass could see the pod that did
	// it, and a page that blanks the line every time the history rolls over is
	// a page that answers "when was the backup last restored" with silence.
	if latest, err := r.latestRehearsal(ctx, db); err == nil && latest != nil {
		db.Status.LastRestore = latest
	}

	ready := metav1.Condition{
		Type: readyCondition, ObservedGeneration: db.Generation, LastTransitionTime: metav1.Now(),
	}
	switch {
	case reconcileErr != nil:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "RenderFailed"
		ready.Message = reconcileErr.Error()
	case apierrors.IsNotFound(setErr):
		ready.Status = metav1.ConditionFalse
		ready.Reason = "AwaitingStatefulSet"
		ready.Message = "the server has not been observed yet"
	case set.Status.ReadyReplicas == 0:
		// Read from the StatefulSet rather than derived from anything here.
		// Ready means PostgreSQL answered pg_isready, which is the only thing
		// that makes the connection string in the Secret worth handing out.
		ready.Status = metav1.ConditionFalse
		ready.Reason = "NotAcceptingConnections"
		ready.Message = "the server is not answering yet"
	default:
		ready.Status = metav1.ConditionTrue
		ready.Reason = "Serving"
		ready.Message = "the server is accepting connections"
	}
	setCondition(&db.Status.Conditions, ready)
	setCondition(&db.Status.Conditions, r.offsiteCondition(ctx, db))

	return r.Status().Update(ctx, db)
}

// offsiteCondition says whether the archives are leaving the cluster, which is
// a different claim from Ready and from the rehearsal both.
//
// "A backup was taken" and "the backup reached somewhere the node's disk is not"
// are two sentences, and folding them into one is how an install spends six
// months believing it has offsite copies. So this reports the second one on its
// own, and its reasons name what to go and fix: a credential, a bucket, a
// network, a full disk.
//
// Absent S3 it is False with reason LocalOnly, and that is not a failure — it is
// the configuration saying what it is. A Database with no destination has
// archives in the same failure domain as its data, and the status says so
// rather than staying quiet and letting the page imply otherwise.
func (r *DatabaseReconciler) offsiteCondition(
	ctx context.Context, db *platformv1alpha1.Database,
) metav1.Condition {
	c := metav1.Condition{
		Type: offsiteCondition, ObservedGeneration: db.Generation,
		LastTransitionTime: metav1.Now(), Status: metav1.ConditionFalse,
	}
	switch {
	case db.Spec.Backup == nil:
		c.Reason = "NoBackups"
		c.Message = "this database is not backed up at all"
		return c
	case db.Spec.Backup.S3 == nil:
		c.Reason = "LocalOnly"
		c.Message = "archives are kept on a volume in this cluster, so they share a " +
			"failure domain with the database; set backup.s3 to copy them off it"
		return c
	}

	failed, err := r.latestShipFailure(ctx, db)
	switch {
	case err != nil:
		// Reported rather than swallowed. Not being able to look and there
		// being nothing wrong are different, and only one of them is fine.
		c.Reason = "Unreadable"
		c.Message = "the backup pods could not be read: " + err.Error()
	case failed != nil:
		// The client's own words, and a reason that names which of them it is.
		c.Reason = shipReason(failed.Reason)
		c.Message = failed.Message
	case db.Status.LastRestore == nil:
		c.Reason = "AwaitingBackup"
		c.Message = "no backup has run yet"
	default:
		c.Status = metav1.ConditionTrue
		c.Reason = "Shipped"
		c.Message = "the most recent archive reached " + db.Spec.Backup.S3.Bucket
	}
	return c
}

// reportedCredentials is the word the ship script prints when S3 refused the
// key rather than the request. Named because the script writes it, this
// function reads it, and the tests assert on it: three places that have to
// agree on one token, and nothing else makes them agree.
const reportedCredentials = "credentials"

// shipReason turns what the ship container reported into a condition reason.
//
// A fixed set, because a reason is what something else keys on — a page, an
// alert, somebody's shell loop — and one that arrives straight from a shell
// script is one a script change can rename without anything noticing.
func shipReason(reported string) string {
	switch reported {
	case reportedCredentials:
		return "CredentialRefused"
	case "bucket":
		return "BucketUnavailable"
	case "network":
		return "EndpointUnreachable"
	case "disk":
		return "NoSpace"
	default:
		return "ShipFailed"
	}
}

// SetupWithManager registers the reconciler.
func (r *DatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Database{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&batchv1.CronJob{}).
		// Not Owns: the backup pods belong to a Job, which belongs to the
		// CronJob. Watching them would be three hops of ownership for an
		// answer the next reconcile reads anyway.
		Named("database").
		Complete(r)
}
