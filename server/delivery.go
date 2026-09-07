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

package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/damgahq/damga/placement"
)

// keyDelivery is what the answer calls the line that says whether anything will
// apply what was just committed. Named because three responses carry it and a
// panel reads it by name.
const keyDelivery = "delivery"

// argoNamespace is where Argo CD watches for Applications.
//
// The chart's own default and the namespace terraform and the installer create.
// Not configurable here yet: an install that put Argo CD somewhere else has one
// more thing to tell this server, and nothing has asked.
const argoNamespace = "argocd"

// Deliverer makes something apply what a deploy commits.
//
// This is the seam the whole product was missing. Every other piece existed:
// the panel picks a catalogue entry, the endpoint plans it, gitwrite commits
// manifests into the tenant's repository — and nothing anywhere turned that
// commit into a running pod. The installer said so in its own words, in the
// list of what it does not install: "Argo CD. Nothing applies what a deploy
// commits."
//
// A seam rather than a hard dependency, for the reason BuildCreator is one: an
// install with no cluster to write to still has to start, serve the panel and
// say what it cannot do. nil means nothing is delivered, and the endpoints that
// would have used it say so rather than reporting a commit as a deployment.
type Deliverer interface {
	// Deliver makes an Argo CD Application for one placement, or updates the
	// one that is already there. It is called after the placement is written
	// and must be safe to call again with the same arguments.
	//
	// The note is empty when the whole chain was written. It is a sentence when
	// the Application exists but something it needs does not — today that is
	// the credential Argo CD reads the repository with, which cannot be written
	// when this install has none. Returned rather than logged because the
	// failure it describes is silent everywhere else: the Application is
	// created, the endpoint answers 201, and Argo CD sits at ComparisonError
	// where nobody is looking.
	Deliver(ctx context.Context, place placement.Placement) (note string, err error)
}

// deliver hands one placement to whatever applies commits, and says plainly
// when there is nothing to hand it to.
//
// An error here never fails the request that produced it. The placement is
// written and the manifests are pushed; what is missing is delivery, and a 500
// would tell the caller to undo work that is fine.
func deliverPlacement(ctx context.Context, st stores, place placement.Placement) (string, error) {
	if st.delivery == nil {
		return "", fmt.Errorf("bu kurulumun teslimatı yok: Argo CD Application'larının yazılacağı " +
			"bir küme olmadan başlatıldı, bu yüzden commit'ler birikiyor ve hiçbir şey onları " +
			"uygulamıyor")
	}
	return st.delivery.Deliver(ctx, place)
}

// applicationName is what the Application for one placement is called.
//
// Derived rather than generated, because this has to be the same name the next
// call produces: delivery is idempotent only if the second call addresses the
// object the first one made.
//
// Namespace and app, and that pair is unique by a constraint rather than by
// convention — the placement store carries UNIQUE (namespace, app). Both are
// DNS labels because the API refuses a placement whose namespace or app is not,
// so the two joined by a dash are a legal object name. The tenant is not in it
// and does not need to be: a namespace belongs to one tenant, which the store
// also enforces.
func applicationName(place placement.Placement) string {
	return place.Namespace + "-" + place.App
}

// clusterDelivery writes Applications into the cluster this server runs in.
type clusterDelivery struct {
	client client.Client
	// auth is where the credential Argo CD is given comes from: the same token
	// this server pushes with, because the repository is the same repository.
	// Two credentials for one repository would be two things to rotate.
	auth GitAuth
}

// Deliver creates or updates the Application, as unstructured rather than
// through Argo CD's Go types.
//
// The types would mean depending on the Argo CD module for four fields, and on
// its transitive graph for the rest of this binary's build. What is written
// here is a document with a known shape; the API server validates it against
// the CRD Argo CD installed, which is the check that matters.
func (d clusterDelivery) Deliver(ctx context.Context, place placement.Placement) (string, error) {
	// The credential first. An Application that arrives before the Secret it
	// needs spends a poll interval at ComparisonError, and the order costs
	// nothing to get right.
	note, err := d.credential(ctx, place)
	if err != nil {
		return "", err
	}

	app := &unstructured.Unstructured{}
	app.SetAPIVersion("argoproj.io/v1alpha1")
	app.SetKind("Application")
	app.SetName(applicationName(place))
	app.SetNamespace(argoNamespace)

	spec := map[string]any{
		"project": "default",
		"source": map[string]any{
			"repoURL": place.RepoURL,
			"path":    place.Path,
			// The branch the platform commits to, or the remote's default when
			// the placement named none — the same rule gitwrite pushes by, so
			// the ref that is written is the ref that is read.
			"targetRevision": revisionOf(place),
		},
		"destination": map[string]any{
			"server":    "https://kubernetes.default.svc",
			"namespace": place.Namespace,
		},
		"syncPolicy": map[string]any{
			// selfHeal is not a convenience here; it is why the fence is a
			// manifest at all. Measured against this project's Argo CD (chart
			// 8.5.10, v3.1.8): a pod-security label removed from a namespace
			// created by CreateNamespace was never restored — not in four
			// minutes, not by a forced sync, not after a hard refresh — and the
			// Application called itself Synced and Healthy throughout. The same
			// label removed from a namespace committed as a manifest came back
			// in about ten seconds. What makes that true is this field.
			//
			// prune, so that a file a render stops producing is an object that
			// stops existing. Without it the write path can add and change but
			// never withdraw, which is the state gitwrite was in until it
			// learned to delete.
			"automated":   map[string]any{"prune": true, "selfHeal": true},
			"syncOptions": []any{"CreateNamespace=false"},
		},
	}
	if err := unstructured.SetNestedMap(app.Object, spec, "spec"); err != nil {
		return "", fmt.Errorf("building the Application: %w", err)
	}

	// Server-side apply, so this owns the fields it sets and leaves anything a
	// human added beside them. Create-then-update would race a second control
	// plane replica through the same endpoint.
	err = d.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(app),
		client.ForceOwnership, client.FieldOwner(fieldOwner))
	if err != nil {
		return "", fmt.Errorf("delivering %s: %w", applicationName(place), err)
	}
	return note, nil
}

// credential writes the Secret Argo CD reads this placement's repository with.
//
// The half of delivery that was missing, and the way it was missing is the
// point: the Application was created, the endpoint answered 201, and Argo CD
// answered
//
//	ComparisonError: failed to list refs: authentication required: Unauthorized
//
// where only somebody already looking at Argo CD would ever see it. Measured on
// a cluster: with the Secret, the same Application reached Synced and the object
// was in the namespace 21 seconds later.
//
// Written from the same GitAuth this server pushes with, because it is the same
// repository. When there is no credential to write — no token configured, or a
// URL tokenAuth refuses — the Application is still delivered and the reason
// comes back as a note. A public repository needs no Secret and that case is
// real, so this is not an error; it is a sentence in the answer.
func (d clusterDelivery) credential(ctx context.Context, place placement.Placement) (string, error) {
	if d.auth == nil {
		return noCredentialNote("bu sunucu git kimlik bilgileri olmadan başlatıldı"), nil
	}
	method, err := d.auth.For(place.RepoURL)
	if err != nil {
		return noCredentialNote(err.Error()), nil
	}
	basic, ok := method.(*githttp.BasicAuth)
	if !ok || basic.Password == "" {
		// A GitAuth another build replaced this one with may authenticate by
		// something an Argo CD repository Secret cannot carry — an SSH key, a
		// token minted per call. Saying so is better than writing a Secret with
		// an empty password, which fails as a bad credential rather than as a
		// missing one.
		return noCredentialNote(fmt.Sprintf(
			"tanımlı git kimlik bilgisi bir %T, ve bunu bir Argo CD depo Secret'ı "+
				"taşıyamaz", method)), nil
	}

	sec := &unstructured.Unstructured{}
	sec.SetAPIVersion("v1")
	sec.SetKind("Secret")
	sec.SetName(repoSecretName(place.RepoURL))
	sec.SetNamespace(argoNamespace)
	// The label is the whole mechanism: Argo CD finds repository credentials by
	// this label and by nothing else, so a Secret without it is a Secret that
	// is never read.
	sec.SetLabels(map[string]string{"argocd.argoproj.io/secret-type": "repository"})
	// data rather than stringData. stringData is write-only — the API server
	// converts it and stores data — so a server-side apply that sends it owns a
	// field that does not exist on the object it just wrote, and the second
	// apply cannot tell what the first one put there.
	if err := unstructured.SetNestedStringMap(sec.Object, map[string]string{
		"type":     base64.StdEncoding.EncodeToString([]byte("git")),
		"url":      base64.StdEncoding.EncodeToString([]byte(place.RepoURL)),
		"username": base64.StdEncoding.EncodeToString([]byte(basic.Username)),
		"password": base64.StdEncoding.EncodeToString([]byte(basic.Password)),
	}, "data"); err != nil {
		return "", fmt.Errorf("building the repository credential: %w", err)
	}

	err = d.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(sec),
		client.ForceOwnership, client.FieldOwner(fieldOwner))
	if err != nil {
		// An error here does fail delivery, unlike the note above. This is the
		// difference between "there was nothing to write" and "there was
		// something to write and it did not land", and the second one leaves an
		// Application that cannot read its own repository.
		return "", fmt.Errorf("writing the repository credential for %s: %w", place.RepoURL, err)
	}
	return "", nil
}

// noCredentialNote is the one sentence the answer carries when Argo CD was
// given nothing to authenticate with.
func noCredentialNote(why string) string {
	return "Argo CD'ye bu depo için hiçbir kimlik bilgisi verilmedi, yani depoyu ancak " +
		"herkese açıksa okuyabilir (" + why + ")"
}

// repoSecretName names the credential after the repository rather than after
// the placement.
//
// One repository, one Secret: Argo CD matches a credential to a repository by
// URL, so a second placement in the same repository wants the same Secret and
// not a second one saying the same thing. The store already enforces that a
// repository belongs to one tenant.
//
// Hashed because a URL is not a name — it carries slashes, colons, dots and a
// length no object name may have. Sixteen hex characters of SHA-256: enough
// that two repositories on one install colliding is not a thing that happens,
// and short enough to read in a `kubectl get secret` listing.
func repoSecretName(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return "damga-repo-" + hex.EncodeToString(sum[:8])
}

// fieldOwner is who these objects belong to, in the eyes of server-side apply.
const fieldOwner = "damga-control-plane"

// clusterDelivery builds the writer this server delivers through.
//
// Its own client rather than the manager's, for the reason clusterReader gives:
// the handler is built before the manager exists, and the manager's client
// reads a cache that is empty until it starts.
func (o Options) clusterDelivery() (Deliverer, error) {
	restCfg := o.RestConfig
	if restCfg == nil {
		var err error
		if restCfg, err = ctrl.GetConfig(); err != nil {
			return nil, fmt.Errorf("delivering a deploy needs a cluster: %w", err)
		}
	}
	// The scheme carries nothing: an Application is written as unstructured,
	// which controller-runtime routes by the GVK on the object itself.
	c, err := client.New(restCfg, client.Options{Scheme: runtime.NewScheme()})
	if err != nil {
		return nil, fmt.Errorf("building the delivery client: %w", err)
	}
	return clusterDelivery{client: c, auth: o.GitAuth}, nil
}

// revisionOf is the ref Argo CD should track for a placement.
func revisionOf(place placement.Placement) string {
	if place.Branch == "" {
		return "HEAD"
	}
	return place.Branch
}
