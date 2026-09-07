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
	"errors"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// The commit the cases build against. A real-looking full sha, because the
// point of most of them is what happens when it is not one.
const (
	testRevision = "0123456789abcdef0123456789abcdef01234567"
	sourceRepo   = "https://github.com/acme/api"

	// Deliberately not defaultRegistry. A case that asserts the default by
	// composing it from the same constant the code reads passes whatever the
	// value is; this one fails if the configured registry is ignored.
	testRegistry = "registry.example.test:5000"
)

// recordingCreator stands in for the cluster. It keeps what it was handed,
// which is the only way to assert that the object the API server would receive
// is the object this package built.
type recordingCreator struct {
	got *platformv1alpha1.Build
	err error
}

func (c *recordingCreator) CreateBuild(_ context.Context, b *platformv1alpha1.Build) error {
	if c.err != nil {
		return c.err
	}
	c.got = b
	// What the API server does with a generated name, done here so the response
	// this handler writes is the one a real create would have produced.
	if b.Name == "" && b.GenerateName != "" {
		b.Name = b.GenerateName + "x7k2q"
	}
	return nil
}

// Always the home tenant's api, for the reason deleteApp gives: what this route
// does from another tenant is covered by TestEveryTenantRouteIsGuarded, which
// walks the table it is in.
func (h *harness) createBuild(account, body string) (int, string) {
	h.t.Helper()
	return h.call(createBuild, http.MethodPost, "/apps/{app}/builds", tenantHome, appAPI, account, body)
}

// Every rule here is a copy of one the CRD enforces, and the copy exists for
// the message rather than for the guarantee. This is what pins that the copies
// still say the same thing as the originals in api/v1alpha1/build_types.go.
func TestBuildForRefusesWhatTheCRDWouldRefuse(t *testing.T) {
	for _, c := range []struct {
		name string
		req  createBuildRequest
	}{
		{"no repository", createBuildRequest{Revision: testRevision}},
		{"a repository that is neither https nor ssh", createBuildRequest{
			Repo: "ftp://example.test/api", Revision: testRevision}},
		// The one most likely to be hit, and the reason the rule exists: a
		// record that says "built main" cannot answer which main.
		{"a branch name where a commit belongs", createBuildRequest{
			Repo: sourceRepo, Revision: branchMain}},
		{"a short sha", createBuildRequest{
			Repo: sourceRepo, Revision: testRevision[:12]}},
		{"a sha in capitals", createBuildRequest{
			Repo: sourceRepo, Revision: strings.ToUpper(testRevision)}},
		{"an absolute path", createBuildRequest{
			Repo: sourceRepo, Revision: testRevision, Path: "/srv/api"}},
		{"a path that climbs out", createBuildRequest{
			Repo: sourceRepo, Revision: testRevision, Path: "svc/../../etc"}},
		{"a builder nothing implements", createBuildRequest{
			Repo: sourceRepo, Revision: testRevision, Builder: "bazel"}},
		{"an image carrying a tag", createBuildRequest{
			Repo: sourceRepo, Revision: testRevision,
			Image: "registry.damga-registry.svc:5000/t_home/api:v1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := buildFor(testRegistry, tenantHome, appAPI, c.req); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// The registry carries a port, so the tag rule has to look at the last path
// segment and not at the whole string.
//
// This exact reference — the platform's own registry — is what the first
// version of the CRD rule refused, on the first build that ever ran. The rule
// is copied here, so the trap is copied here too unless something holds it.
func TestBuildForAcceptsARegistryWithAPort(t *testing.T) {
	got, err := buildFor(testRegistry, tenantHome, appAPI, createBuildRequest{
		Repo: sourceRepo, Revision: testRevision,
		Image: "registry.damga-registry.svc:5000/ci/app",
	})
	if err != nil {
		t.Fatalf("a registry with a port was refused: %v", err)
	}
	if got.Spec.Image != "registry.damga-registry.svc:5000/ci/app" {
		t.Errorf("Image = %q", got.Spec.Image)
	}
}

// What a request with nothing optional in it becomes.
func TestBuildForFillsInWhatTheRequestLeftOut(t *testing.T) {
	got, err := buildFor(testRegistry, tenantHome, appAPI, createBuildRequest{
		Repo: sourceRepo, Revision: testRevision,
	})
	if err != nil {
		t.Fatalf("buildFor: %v", err)
	}

	// Never beside the application being built. cluster/build-namespace.yaml
	// has the measurement that forced it: builds run privileged, so the
	// containment is the namespace.
	if got.Namespace != BuildNamespace {
		t.Errorf("Namespace = %q, want %q", got.Namespace, BuildNamespace)
	}
	// Generated, because rebuilding the same commit is a new Build — the spec
	// is immutable, so a name derived from the commit would make the second
	// attempt a conflict on an object nobody can edit.
	if got.Name != "" {
		t.Errorf("Name = %q, want it left for the API server to generate", got.Name)
	}
	if want := "api-" + testRevision[:12] + "-"; got.GenerateName != want {
		t.Errorf("GenerateName = %q, want %q", got.GenerateName, want)
	}
	if got.Spec.Builder != platformv1alpha1.BuildDetect {
		t.Errorf("Builder = %q, want it set rather than left for the CRD default", got.Spec.Builder)
	}
	if want := testRegistry + "/" + tenantHome + "/api"; got.Spec.Image != want {
		t.Errorf("Image = %q, want %q", got.Spec.Image, want)
	}
	// Every build of every tenant shares one namespace, so the object itself
	// has to say whose it is.
	if got.Labels["damga.co/tenant"] != tenantHome || got.Labels["damga.co/app"] != appAPI {
		t.Errorf("labels do not say whose build this is: %v", got.Labels)
	}
	if got.Labels["damga.co/revision"] != testRevision {
		t.Errorf("revision label = %q", got.Labels["damga.co/revision"])
	}
}

// The endpoint cannot work in this build, and it says which kind of "cannot".
//
// "This installation cannot start builds" and "the build could not be started"
// send a reader to two different places: the first to the chart, where the
// permission is missing, and the second to the build itself. backup.go makes
// the same distinction for the same reason — an empty answer would read as
// "no backups", which is the one thing it must not be mistaken for.
func TestCreateBuildWithNoClusterSaysSoRatherThanFailing(t *testing.T) {
	h := newHarness(t)
	if h.stores.builds != nil {
		t.Fatal("this case is about the seam being empty and it is not")
	}

	code, body := h.createBuild(accMember, `{
		"repo":"https://github.com/acme/api","revision":"`+testRevision+`"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("POST /builds with no cluster = %d, want 501: %s", code, body)
	}
	if !strings.Contains(body, BuildNamespace) {
		t.Errorf("the refusal does not say where the permission is missing: %s", body)
	}
}

// A malformed request is answered before the seam is consulted, so a caller
// finds out that their sha is a branch name even on an install that cannot
// build anything at all.
func TestCreateBuildValidatesBeforeItGivesUp(t *testing.T) {
	h := newHarness(t)
	code, body := h.createBuild(accMember, `{
		"repo":"https://github.com/acme/api","revision":"main"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a branch name where a commit belongs = %d, want 400: %s", code, body)
	}
}

// With the seam filled, the cluster is handed exactly what buildFor produced
// and the caller is told where to look for the answer.
func TestCreateBuildHandsTheClusterWhatItValidated(t *testing.T) {
	h := newHarness(t)
	creator := &recordingCreator{}
	h.stores.builds = creator

	code, body := h.createBuild(accMember, `{
		"repo":"https://github.com/acme/api","revision":"`+testRevision+`",
		"path":"services/api","builder":"dockerfile"}`)
	// Accepted and not created: a Build is a request a job will answer, and the
	// digest arrives minutes later through its status.
	if code != http.StatusAccepted {
		t.Fatalf("POST /builds = %d, want 202: %s", code, body)
	}
	if creator.got == nil {
		t.Fatal("the handler answered without creating anything")
	}
	switch {
	case creator.got.Namespace != BuildNamespace:
		t.Errorf("Namespace = %q", creator.got.Namespace)
	case creator.got.Spec.Repo != sourceRepo:
		t.Errorf("Repo = %q", creator.got.Spec.Repo)
	case creator.got.Spec.Revision != testRevision:
		t.Errorf("Revision = %q", creator.got.Spec.Revision)
	case creator.got.Spec.Path != "services/api":
		t.Errorf("Path = %q", creator.got.Spec.Path)
	case creator.got.Spec.Builder != platformv1alpha1.BuildDockerfile:
		t.Errorf("Builder = %q", creator.got.Spec.Builder)
	}
	// The name the API server assigned, so the caller can find the build again
	// without listing a namespace it cannot read.
	if !strings.Contains(body, creator.got.Name) {
		t.Errorf("the response does not name the build it created: %s", body)
	}
}

// The resource these cases name, which is also the one the endpoint creates.
const buildsResource = "builds"

// A cluster that refuses the create because the control plane has not been
// granted the right is not a bad gateway. It is the same missing permission the
// empty seam stands for, arriving one step later, and it is fixed in the same
// place — so it gets the same answer rather than one that reads as an outage.
func TestCreateBuildTranslatesAForbiddenIntoTheMissingPermission(t *testing.T) {
	h := newHarness(t)
	h.stores.builds = &recordingCreator{err: apierrors.NewForbidden(
		schema.GroupResource{Group: "platform.damga.co", Resource: buildsResource}, "",
		errors.New("builds.platform.damga.co is forbidden"))}

	code, body := h.createBuild(accMember, `{
		"repo":"https://github.com/acme/api","revision":"`+testRevision+`"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("a Forbidden from the API server = %d, want 501: %s", code, body)
	}
	if !strings.Contains(body, BuildNamespace) {
		t.Errorf("the refusal does not say where the permission is missing: %s", body)
	}
}

// Building is the first half of deploying, so the right to start one is
// app:deploy — which a viewer does not have. Splitting them would let somebody
// spend the cluster's build quota without being able to use the result.
func TestCreateBuildRefusesAViewer(t *testing.T) {
	h := newHarness(t)
	h.stores.builds = &recordingCreator{}

	code, _ := h.createBuild(accViewer, `{
		"repo":"https://github.com/acme/api","revision":"`+testRevision+`"}`)
	if code != http.StatusForbidden {
		t.Fatalf("a viewer started a build: %d", code)
	}
	if h.stores.builds.(*recordingCreator).got != nil {
		t.Error("a refused request reached the cluster")
	}
}

// A full namespace is not a missing permission, and until this existed it was
// reported as one.
//
// Both arrive as Forbidden, so the endpoint answered 501 "the control plane is
// not permitted to create builds in damga-build" — a sentence that is true of a
// missing RoleBinding, sends whoever reads it to the chart, and is what a user
// would have seen on the two hundredth build of an installation whose records
// nothing was collecting. It is the shape this repository has lost rounds to
// twice: one sentence that is true for more than one cause.
//
// The three refusals here are the three causes, and each has to be told apart
// from the other two.
func TestCreateBuildTellsAFullQuotaApartFromAMissingPermission(t *testing.T) {
	// The answer the other two causes must not produce, named because it is
	// the whole point: for a long time it was the answer to all three.
	const notPermitted = "izni yok"

	forbidden := func(detail string) error {
		return apierrors.NewForbidden(
			schema.GroupResource{Group: "platform.damga.co", Resource: buildsResource}, "",
			errors.New(detail))
	}

	for _, tc := range []struct {
		what   string
		err    error
		code   int
		says   string
		unsaid string
	}{
		{
			what: "the namespace is full",
			// The admission plugin's wording. Read from Kubernetes rather than
			// measured here: a quota is enforced only where a controller
			// computes its usage, and envtest runs none — measured, five
			// creates against a quota of two all succeeded. The CI step that
			// fills a real quota is where this string is checked.
			err: forbidden("exceeded quota: build-quota, requested: " +
				"count/builds.platform.damga.co=1, used: count/builds.platform.damga.co=200, " +
				"limited: count/builds.platform.damga.co=200"),
			code:   http.StatusServiceUnavailable,
			says:   "yer yok",
			unsaid: notPermitted,
		},
		{
			what: "the quota cannot count the type",
			// This one is measured, by this repository, in the CI round it
			// cost: cluster/build-namespace.yaml records the message and the
			// ordering that fixes it.
			err:    forbidden("status unknown for quota: build-quota"),
			code:   http.StatusInternalServerError,
			says:   "onu sayan kotadan önce",
			unsaid: notPermitted,
		},
		{
			what:   "the control plane may not create builds",
			err:    forbidden("builds.platform.damga.co is forbidden"),
			code:   http.StatusNotImplemented,
			says:   notPermitted,
			unsaid: "kota",
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			h := newHarness(t)
			h.stores.builds = &recordingCreator{err: tc.err}

			code, body := h.createBuild(accMember, `{
				"repo":"https://github.com/acme/api","revision":"`+testRevision+`"}`)
			if code != tc.code {
				t.Errorf("%s = %d, want %d: %s", tc.what, code, tc.code, body)
			}
			if !strings.Contains(body, tc.says) {
				t.Errorf("the answer does not name this cause (%q): %s", tc.says, body)
			}
			if strings.Contains(body, tc.unsaid) {
				t.Errorf("the answer describes a different cause (%q): %s", tc.unsaid, body)
			}
		})
	}
}
