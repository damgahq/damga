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

package buildwatch

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

const (
	tenantHome = "t_home"
	appAPI     = "api"
)

type call struct {
	tenant, app, env, image string
}

type recorder struct {
	calls []call
	err   error
	wrote bool
}

func (r *recorder) Deploy(_ context.Context, tenant, app, env, image string) (bool, error) {
	r.calls = append(r.calls, call{tenant, app, env, image})
	return r.wrote, r.err
}

func build(name string, phase platformv1alpha1.BuildPhase, digest string, labels map[string]string) *platformv1alpha1.Build {
	return &platformv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "damga-build", Labels: labels,
			UID: types.UID("uid-" + name),
		},
		Spec:   platformv1alpha1.BuildSpec{Repo: "https://example.test/x.git", Revision: "0123456789abcdef0123456789abcdef01234567", Image: "reg.test/t/app"},
		Status: platformv1alpha1.BuildStatus{Phase: phase, Digest: digest},
	}
}

func labelled() map[string]string {
	return map[string]string{
		platformv1alpha1.BuildTenantLabel: tenantHome,
		platformv1alpha1.BuildAppLabel:    appAPI,
		EnvLabel:                          "prod",
	}
}

func run(t *testing.T, b *platformv1alpha1.Build, d Deployer) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(b).Build()
	r := &Reconciler{Client: c, Deployer: d}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: b.Name, Namespace: b.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return r
}

// The joint this package is: a digest in a status field becomes a commit.
func TestASucceededBuildIsDeployedAsItsDigest(t *testing.T) {
	rec := &recorder{wrote: true}
	run(t, build("b1", platformv1alpha1.BuildSucceeded, "sha256:abc", labelled()), rec)

	if len(rec.calls) != 1 {
		t.Fatalf("a succeeded build with a digest was not deployed: %+v", rec.calls)
	}
	got := rec.calls[0]
	if got.tenant != tenantHome || got.app != appAPI || got.env != "prod" {
		t.Errorf("deployed to the wrong place: %+v", got)
	}
	// The digest and not the tag. A tag can be moved; that is the entire
	// reason the Build reports a digest at all.
	if got.image != "reg.test/t/app@sha256:abc" {
		t.Errorf("deployed %q, want the image pinned to the digest the build produced", got.image)
	}
}

// Every reason not to deploy, kept apart.
func TestABuildIsNotDeployedWhenItShouldNotBe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build *platformv1alpha1.Build
		why   string
	}{
		{
			name:  "still running",
			build: build("b", platformv1alpha1.BuildRunning, "", labelled()),
			why:   "a build that has not finished was deployed",
		},
		{
			name:  "failed",
			build: build("b", platformv1alpha1.BuildFailed, "", labelled()),
			why:   "a failed build was deployed",
		},
		{
			name:  "succeeded with no digest",
			build: build("b", platformv1alpha1.BuildSucceeded, "", labelled()),
			why: "a build that produced no digest was deployed, which would replace a " +
				"running image with an empty reference",
		},
		{
			name: "no environment label",
			build: build("b", platformv1alpha1.BuildSucceeded, "sha256:abc", map[string]string{
				platformv1alpha1.BuildTenantLabel: tenantHome,
				platformv1alpha1.BuildAppLabel:    appAPI,
			}),
			why: "a build that names no environment was deployed somewhere; there is no " +
				"safe default, and the wrong guess ships a branch into production",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			run(t, tc.build, rec)
			if len(rec.calls) != 0 {
				t.Errorf("%s: %+v", tc.why, rec.calls)
			}
		})
	}
}

// The repeat, which has no object to be recorded on.
//
// The ClusterRole holds no patch on builds, so there is nowhere to mark one as
// handled. Every resync offers the same succeeded build again, and gitwrite
// cannot refuse it — Render stamps a fresh rollout id, so the bytes always
// differ and ErrNoChange never fires. Without this the platform would commit
// the same digest once per resync, for ever.
func TestASucceededBuildIsDeployedOnceAndNotOncePerResync(t *testing.T) {
	rec := &recorder{wrote: true}
	b := build("b1", platformv1alpha1.BuildSucceeded, "sha256:abc", labelled())
	r := run(t, b, rec)

	for range 5 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: b.Name, Namespace: b.Namespace},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if len(rec.calls) != 1 {
		t.Errorf("the same build was deployed %d times; every resync would be a commit",
			len(rec.calls))
	}
}

// A failure is retried, and is deliberately not remembered as done.
func TestADeployThatFailedIsTriedAgain(t *testing.T) {
	rec := &recorder{err: errors.New("the forge was unreachable")}
	b := build("b1", platformv1alpha1.BuildSucceeded, "sha256:abc", labelled())

	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(b).Build()
	r := &Reconciler{Client: c, Deployer: rec}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: b.Name, Namespace: b.Namespace}}

	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("a failed deploy returned no error, so controller-runtime would not retry it")
	}
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("the second attempt returned no error")
	}
	if len(rec.calls) != 2 {
		t.Errorf("a build whose deploy failed was marked done and never retried: %d attempts",
			len(rec.calls))
	}
}
