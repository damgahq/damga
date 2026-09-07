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

// Package buildwatch turns a finished build into a deploy.
//
// The second of three joints in `git push -> build -> deploy` that were each
// built and never connected. build_controller sets BuildSucceeded and writes
// the digest the build produced, and stopped there: the digest sat in a status
// field that nothing read, so a webhook could start a build, the build could
// succeed, and nothing ran.
//
// # Why this is in front of the write path rather than inside it
//
// docs/PLAN.md §3 states the rule this package is the implementation of: a
// build produces a digest, the digest is written into a commit, and Argo CD
// applies it — the build stands in FRONT of the write path, not inside it. So
// this package does not touch the cluster's desired state. It reads a status
// and asks for a commit, which is the only way anything here changes what runs.
package buildwatch

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// EnvLabel is which environment a build is for.
//
// It is not in api/v1alpha1 beside the tenant and app labels, and that is not
// tidiness: those constants are the operator's, and this one is written and
// read entirely by the server. It lives here because this package is the
// reader, and the writer imports it — the other direction would be a cycle,
// since deploying needs the render that lives in the server.
//
// A build without it is a build nothing can deploy, and that is the honest
// state for one: POST /apps/{app}/builds is per app and names no environment,
// so a build asked for by hand has no destination until somebody says which.
const EnvLabel = "damga.co/env"

// Deployer commits a digest as the image an environment runs.
//
// A seam, and it is what keeps this package out of an import cycle: rendering
// a Workload and pushing a commit both live in the server, and the server is
// what constructs this reconciler. It is also what makes the reconciler
// testable without a git remote.
type Deployer interface {
	// Deploy writes image as this environment's image, and reports whether it
	// wrote anything. false with a nil error means the environment already runs
	// this image — see the note on repetition in Reconcile.
	Deploy(ctx context.Context, tenantID, app, env, image string) (bool, error)
}

// Reconciler watches Builds and deploys the ones that produced a digest.
type Reconciler struct {
	client.Client
	Deployer Deployer

	// done remembers which builds have already been deployed.
	//
	// A cache and not the answer. The ClusterRole this runs under holds get,
	// list, watch and create on builds and deliberately not patch, so there is
	// nowhere on the object to record that it has been handled — which is the
	// right grant and the reason this field exists. Every resync would
	// otherwise re-deploy every succeeded build, and gitwrite cannot refuse the
	// repetition: manifest.Render stamps a fresh rollout id on every render, so
	// the bytes always differ and ErrNoChange can never fire through a server
	// path. A loop, not a no-op.
	//
	// Correctness does not rest on this. It is lost on restart, and what
	// catches the repeat then is the Deployer, which compares the committed
	// image to the digest before it writes. This only keeps the common case
	// from paying for a clone on every resync.
	done sync.Map
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Build{}).
		Named("buildwatch").
		Complete(r)
}

// The permission this needs, and it is a read. The write is a commit.
//
// +kubebuilder:rbac:groups=platform.damga.co,resources=builds,verbs=get;list;watch

// Reconcile deploys a build that succeeded, once.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var build platformv1alpha1.Build
	if err := r.Get(ctx, req.NamespacedName, &build); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only a build that finished and said what it produced. A succeeded build
	// with no digest is the case build_controller reports as NoDigest, and
	// deploying an empty image would replace a running one with nothing.
	if build.Status.Phase != platformv1alpha1.BuildSucceeded || build.Status.Digest == "" {
		return ctrl.Result{}, nil
	}

	tenantID := build.Labels[platformv1alpha1.BuildTenantLabel]
	app := build.Labels[platformv1alpha1.BuildAppLabel]
	env := build.Labels[EnvLabel]
	if tenantID == "" || app == "" || env == "" {
		// Not an error, and not retried. A build made by POST .../builds names
		// no environment, and there is no default that would be safe to guess:
		// the wrong one deploys a branch into production. Logged once, because
		// a build nobody can deploy is worth seeing.
		if _, seen := r.done.LoadOrStore(key(build.UID), true); !seen {
			log.Info("a build produced a digest and names no environment to deploy it to",
				"build", build.Name, "tenant", tenantID, "app", app,
				"digest", build.Status.Digest)
		}
		return ctrl.Result{}, nil
	}

	if _, seen := r.done.Load(key(build.UID)); seen {
		return ctrl.Result{}, nil
	}

	// The digest and not the tag. That is the whole point of the Build
	// resource: a tag can be moved and a digest cannot, so what gets committed
	// is the thing that cannot change underneath the record.
	image := build.Spec.Image + "@" + build.Status.Digest

	wrote, err := r.Deployer.Deploy(ctx, tenantID, app, env, image)
	if err != nil {
		// Retried, and deliberately not marked done. A commit that failed
		// because the forge was unreachable is exactly the case that should be
		// tried again.
		return ctrl.Result{}, fmt.Errorf("deploying %s: %w", build.Name, err)
	}
	r.done.Store(key(build.UID), true)
	if wrote {
		log.Info("a build became a deploy", "build", build.Name, "tenant", tenantID,
			"app", app, "env", env, "image", image)
	}
	return ctrl.Result{}, nil
}

func key(uid types.UID) string { return string(uid) }
