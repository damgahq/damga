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
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/damgahq/damga/placement"
)

// What an apply carried, without naming controller-runtime's unexported type.
//
// ApplyConfigurationFromUnstructured wraps the object in something that embeds
// *unstructured.Unstructured, so everything worth asserting is reachable
// through the methods it inherits.
type appliedObject interface {
	GetAPIVersion() string
	GetKind() string
	GetName() string
	GetNamespace() string
	GetLabels() map[string]string
	UnstructuredContent() map[string]any
}

// applyRecorder is the cluster, minus the cluster.
//
// The embedded client.Client is nil on purpose: any method this test has not
// thought about panics rather than quietly returning a zero value, which is the
// difference between a test double and a place where behaviour goes to hide.
type applyRecorder struct {
	client.Client
	applied []appliedObject
	err     error
}

func (r *applyRecorder) Apply(_ context.Context, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
	if r.err != nil {
		return r.err
	}
	got, ok := obj.(appliedObject)
	if !ok {
		return errors.New("the object applied was not an unstructured document")
	}
	r.applied = append(r.applied, got)
	return nil
}

// The values these tests are about are the repository and the token; everything
// else is the same placement every other test in this package uses.
const (
	credentialRepo      = "https://example.test/acme/state.git"
	credentialToken     = "ghp_secret"
	credentialNamespace = "acme-state"
)

func testPlacement(repo string) placement.Placement {
	return placement.Placement{
		TenantID: "t_1", App: appAPI, Env: envProd,
		RepoURL: repo, Branch: branchMain, Path: lifeDir, Namespace: credentialNamespace,
	}
}

func decode(t *testing.T, obj appliedObject, key string) string {
	t.Helper()
	data, ok := obj.UnstructuredContent()["data"].(map[string]any)
	if !ok {
		t.Fatalf("the credential carries no data block at all: %v", obj.UnstructuredContent())
	}
	raw, ok := data[key].(string)
	if !ok {
		t.Fatalf("the credential has no %s", key)
	}
	out, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("%s is not base64, so the API server will refuse the Secret: %v", key, err)
	}
	return string(out)
}

// The half of delivery that was missing, and the way it was missing.
//
// Measured on a cluster before this existed: the control plane wrote the
// Application, the endpoint answered 201, and Argo CD answered
// "ComparisonError: failed to list refs: authentication required: Unauthorized"
// — a private repository it had no credential for. Nothing in the panel, the
// record or the endpoint said so. With the Secret, the same Application reached
// Synced and the object was in the cluster twenty-one seconds later.
func TestDeliveryWritesTheCredentialArgoCDReadsTheRepositoryWith(t *testing.T) {
	rec := &applyRecorder{}
	d := clusterDelivery{client: rec, auth: tokenAuth{token: credentialToken}}

	note, err := d.Deliver(context.Background(), testPlacement(credentialRepo))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if note != "" {
		t.Errorf("a fully delivered app came back with a note: %q", note)
	}
	if len(rec.applied) != 2 {
		t.Fatalf("delivery applied %d objects, want the credential and the Application",
			len(rec.applied))
	}

	// The credential first. An Application that arrives before it spends a poll
	// interval at ComparisonError for no reason.
	secret, app := rec.applied[0], rec.applied[1]
	if secret.GetKind() != "Secret" {
		t.Fatalf("the first object applied is a %s; the Application was written before the "+
			"credential it needs", secret.GetKind())
	}
	if app.GetKind() != "Application" {
		t.Fatalf("the second object applied is a %s", app.GetKind())
	}

	// Named after the repository and not after the placement. Asserted here and
	// not only against repoSecretName, because the first version of this test
	// checked the helper and nothing checked that Deliver used it: a name built
	// from the namespace and the app passed every assertion while leaving one
	// credential per environment.
	if got, want := secret.GetName(), repoSecretName(credentialRepo); got != want {
		t.Errorf("the credential is called %q and not %q, so a second environment in the "+
			"same repository writes a second Secret saying the same thing", got, want)
	}
	if got := secret.GetNamespace(); got != argoNamespace {
		t.Errorf("the credential was written to %q, where Argo CD is not looking", got)
	}
	// The label is the entire mechanism. Argo CD finds repository credentials by
	// this label and by nothing else, so a Secret without it is a Secret that is
	// never read — and everything else here would still look right.
	if got := secret.GetLabels()["argocd.argoproj.io/secret-type"]; got != "repository" {
		t.Errorf("argocd.argoproj.io/secret-type is %q, so Argo CD never reads this Secret "+
			"and the Application beside it cannot read its repository", got)
	}
	if got := decode(t, secret, "url"); got != credentialRepo {
		t.Errorf("the credential is for %q, which is not the repository being delivered", got)
	}
	if got := decode(t, secret, "password"); got != credentialToken {
		t.Errorf("the credential carries %q as the password", got)
	}
	if got := decode(t, secret, "type"); got != "git" {
		t.Errorf("the credential type is %q, and Argo CD reads only git", got)
	}
}

// One repository is one credential, and the same repository is the same object.
//
// Argo CD matches a credential to a repository by URL, so a second placement in
// the same repository wants the Secret that is already there. A name derived
// from the placement would leave one Secret per environment, all of them saying
// the same thing, and rotating the token would mean finding all of them.
func TestOneRepositoryIsOneCredential(t *testing.T) {
	const repo = credentialRepo
	first := repoSecretName(repo)
	if first != repoSecretName(repo) {
		t.Fatal("the same repository produced two different Secret names, so delivery is not " +
			"idempotent and every deploy leaves another credential behind")
	}
	if first == repoSecretName("https://example.test/acme/other.git") {
		t.Fatal("two repositories share one Secret name; one tenant's token would be " +
			"overwritten by another's")
	}
	if strings.ContainsAny(first, "/:.") || len(first) > 63 {
		t.Errorf("%q is not a legal object name", first)
	}
}

// No token is not an error, and it is not silence either.
//
// A public repository needs no credential and that case is real. What is not
// acceptable is delivering an Application that cannot read its repository and
// answering 201 with nothing said — so the reason comes back as a note, and the
// panel prints it under the app it was creating.
func TestWithNoTokenTheAnswerSaysWhyArgoCDCannotRead(t *testing.T) {
	rec := &applyRecorder{}
	d := clusterDelivery{client: rec, auth: noAuth{}}

	note, err := d.Deliver(context.Background(), testPlacement(credentialRepo))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(rec.applied) != 1 || rec.applied[0].GetKind() != "Application" {
		t.Fatalf("with no credential to write, delivery applied %d objects", len(rec.applied))
	}
	if !strings.Contains(note, "-git-token-file") {
		t.Errorf("the note is %q, and does not name the flag that would fix it", note)
	}
}

// The same, for a repository this build cannot authenticate to at all. The
// reason is quoted rather than replaced: "only https repositories are
// supported" names the URL, and a note saying "no credential" would send the
// reader looking for a missing token.
func TestASchemeThatCannotBeAuthenticatedIsQuoted(t *testing.T) {
	rec := &applyRecorder{}
	d := clusterDelivery{client: rec, auth: tokenAuth{token: credentialToken}}

	note, err := d.Deliver(context.Background(), testPlacement("git://git.internal:9418/state.git"))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(rec.applied) != 1 {
		t.Fatalf("a credential was written for a scheme nothing can authenticate to")
	}
	if !strings.Contains(note, "yalnızca https depoları destekleniyor") {
		t.Errorf("the note is %q and does not say what was actually wrong", note)
	}
}

// A credential that could not be written is a failure, unlike a credential
// there was nothing to write. The difference matters: the first leaves an
// Application that cannot read its own repository, and answering 201 for it is
// the exact failure this whole file exists to close.
func TestACredentialThatCannotBeWrittenFailsTheDelivery(t *testing.T) {
	rec := &applyRecorder{err: errors.New("secrets is forbidden")}
	d := clusterDelivery{client: rec, auth: tokenAuth{token: credentialToken}}

	_, err := d.Deliver(context.Background(), testPlacement(credentialRepo))
	if err == nil {
		t.Fatal("delivery succeeded with no credential written")
	}
	if !strings.Contains(err.Error(), credentialRepo) {
		t.Errorf("the error is %q and does not name the repository it is about", err)
	}
}
