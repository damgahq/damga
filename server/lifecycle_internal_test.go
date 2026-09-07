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

// In package server because these drive handlers the route table holds and
// reach into stores to substitute a writer, neither of which is exported.
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

const (
	lifeDir  = "apps/api/prod"
	imageOne = "ghcr.io/acme/api:1"
	imageTwo = "ghcr.io/acme/api:2"
)

// passthroughAuth is what an install supplies for a repository on a private
// network that needs no credential. noAuth, the default, refuses — which is
// right for a real install with no token and useless here.
type passthroughAuth struct{}

func (passthroughAuth) For(string) (transport.AuthMethod, error) { return nil, nil }

// lifecycle wires the harness for the git write path: a real bare repository, a
// writer over the harness's evidence store, and a placement pointing at both.
type lifecycle struct {
	*harness
	repo string
}

func newLifecycle(t *testing.T) *lifecycle {
	t.Helper()
	h := newHarness(t)
	repo := stateRepo(t)

	h.stores.writer = &gitwrite.Writer{Evidence: h.records}
	h.stores.gitAuth = passthroughAuth{}
	if _, err := h.places.Put(context.Background(), placement.Placement{
		TenantID: tenantHome, App: appAPI, Env: envProd,
		RepoURL: repo, Branch: branchMain, Path: lifeDir, Namespace: nsHomeProd,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return &lifecycle{harness: h, repo: repo}
}

func (l *lifecycle) rollback(seq, account string) (int, string) {
	l.t.Helper()
	return l.callSeq(rollbackRoute, "/apps/{app}/envs/{env}/deploys/{seq}/rollback", seq, account, "")
}

func (l *lifecycle) scale(account, body string) (int, string) {
	l.t.Helper()
	return l.callSeq(scaleRoute, "/apps/{app}/envs/{env}/scale", "", account, body)
}

func (l *lifecycle) restart(account string) (int, string) {
	l.t.Helper()
	return l.callSeq(restartRoute, "/apps/{app}/envs/{env}/restart", "", account, "")
}

// observe does what the deploy observer does in a real cluster: it records the
// image the workload actually ran.
//
// The git write path does not. Writer.Deploy opens the record with the ref, the
// actor and the source, and evidence.Image is learned afterwards from the live
// Deployment — deploywatch reads it off the container and merges it in on a
// transition. So a record carries no image until something observed it, and a
// rollback has nothing to restore until then. That is a real property of an
// install running with ObserveDeploys off, not an artefact of these cases;
// TestARollbackOnAnUnobservedDeployRefusesRatherThanGuessing is the one that
// says so out loud.
func (l *lifecycle) observe(image string) {
	l.t.Helper()
	ctx := context.Background()
	page, err := l.records.History(ctx, evidence.Query{
		Ref:   evidence.Ref{TenantID: tenantHome, App: appAPI, Env: envProd},
		Order: evidence.OrderNewest, Limit: 1,
	})
	if err != nil || len(page.Records) == 0 {
		l.t.Fatalf("reading the record to observe: %v", err)
	}
	rec := page.Records[0]
	version := len(rec.Transitions)
	if _, err := l.records.Transition(ctx, rec.ID, evidence.Transition{
		From: []evidence.State{rec.State}, To: evidence.StateRunning,
		At: time.Now().UTC(), Reason: "observed running",
		Observation: evidence.Observation{
			Source: evidence.ObservedFromWorkload, At: time.Now().UTC(),
		},
		Image:        &evidence.Image{RequestedRef: image},
		ExpectEvents: &version,
	}); err != nil {
		l.t.Fatalf("observing %s: %v", rec.ID, err)
	}
}

// deployed is a deploy followed by the observation that closes it, which is
// what a cluster produces and what a rollback needs to find.
//
// Always as a member: the cases that turn on who is asking call the endpoints
// directly, and one that set this up as a viewer would be testing the guard
// through a fixture instead of through the handler.
func (l *lifecycle) deployed(image, body string) {
	l.t.Helper()
	if code, resp := l.deploy(body); code != http.StatusAccepted {
		l.t.Fatalf("deploy %s = %d: %s", image, code, resp)
	}
	l.observe(image)
}

// deploy is setup and never the subject: what a deploy does for a viewer is
// deploy.go's to prove, and driving it as anyone but a member here would be
// asserting somebody else's rule through this file's fixtures.
func (l *lifecycle) deploy(body string) (int, string) {
	l.t.Helper()
	return l.callSeq(deployRoute, "/apps/{app}/envs/{env}/deploys", "", accMember, body)
}

// callSeq is harness.call with {env} and {seq} filled in too. The harness's own
// helper substitutes only {tenant} and {app}, and driving through a mux is what
// sets the path values the guard and the handlers read — calling a handler
// directly leaves every one of them empty and the cases pass for nothing.
func (l *lifecycle) callSeq(
	handler func(guard, stores) http.Handler, suffix, seq, account, body string,
) (int, string) {
	l.t.Helper()
	mux := http.NewServeMux()
	mux.Handle(http.MethodPost+" "+tenantScope+suffix, handler(l.guard, l.stores))

	target := strings.NewReplacer(
		"{tenant}", tenantHome, "{app}", appAPI, "{env}", envProd, "{seq}", seq,
	).Replace(tenantScope + suffix)
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Host = testHost
	if account != "" {
		req.AddCookie(l.cookies[account])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// ---------------------------------------------------------------- git helpers

// stateRepo makes a bare repository with one commit on main and returns its
// path. A bare repository has no worktree and an empty one cannot be cloned, so
// the first commit is made beside it and pushed.
func stateRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "state.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init: %v", err)
	}
	work := filepath.Join(dir, seedName)
	seed, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatalf("seed init: %v", err)
	}
	if _, err := seed.CreateRemote(&gitconfig.RemoteConfig{
		Name: "origin", URLs: []string{bare},
	}); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	tree, err := seed.Worktree()
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("state\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := tree.Add("README.md"); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	who := &object.Signature{Name: seedName, Email: seedEmail, When: time.Now()}
	if _, err := tree.Commit(seedName, &git.CommitOptions{Author: who, Committer: who}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	head, err := seed.Head()
	if err != nil {
		t.Fatalf("seed head: %v", err)
	}
	if err := seed.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchMain), head.Hash()),
	); err != nil {
		t.Fatalf("seed branch: %v", err)
	}
	if err := seed.Push(&git.PushOptions{RefSpecs: []gitconfig.RefSpec{
		gitconfig.RefSpec("refs/heads/main:refs/heads/main"),
	}}); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	return bare
}

// seedWorkload pushes a rendered Workload into the placement's directory, for
// the cases whose subject is a manifest this API cannot produce — one that
// autoscales, or one with a volume.
func seedWorkload(t *testing.T, repoPath string, app platformv1alpha1.Workload) {
	t.Helper()
	body, err := manifest.Render(app, "seeded")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	dir := t.TempDir()
	// The branch is named rather than left to HEAD. PlainInit on a bare
	// repository points HEAD at refs/heads/master, which this seed never
	// creates, and a clone that resolves HEAD first fails with
	// "reference not found" before it looks at anything.
	clone, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL: repoPath, ReferenceName: plumbing.NewBranchReferenceName(branchMain),
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	tree, err := clone.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	full := filepath.Join(dir, lifeDir)
	if err := os.MkdirAll(full, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(full, manifest.File), body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := tree.Add(filepath.Join(lifeDir, manifest.File)); err != nil {
		t.Fatalf("add: %v", err)
	}
	who := &object.Signature{Name: seedName, Email: seedEmail, When: time.Now()}
	if _, err := tree.Commit("seed a workload", &git.CommitOptions{Author: who, Committer: who}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := clone.Push(&git.PushOptions{}); err != nil {
		t.Fatalf("push: %v", err)
	}
}

// committedWorkload parses the manifest as it stands on main.
func committedWorkload(t *testing.T, repoPath string) platformv1alpha1.Workload {
	t.Helper()
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchMain), true)
	if err != nil {
		t.Fatalf("main: %v", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	f, err := commit.File(lifeDir + "/" + manifest.File)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	body, err := f.Contents()
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	app, err := manifest.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return app
}

// commitsOnMain counts what the branch carries, which is how a rollback is told
// apart from a rewind.
func commitsOnMain(t *testing.T, repoPath string) int {
	t.Helper()
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchMain), true)
	if err != nil {
		t.Fatalf("main: %v", err)
	}
	iter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	defer iter.Close()
	n := 0
	if err := iter.ForEach(func(*object.Commit) error { n++; return nil }); err != nil {
		t.Fatalf("walk: %v", err)
	}
	return n
}

// ---------------------------------------------------------------- rollback

// A rollback moves the branch forward carrying an old image. It is not a
// rewind, and this is the case that says so.
//
// Argo CD tracks a branch. Pointing it at an older revision — or force-pushing
// the branch back — makes selfHeal fight whoever moved it: the cluster is
// reconciled towards what the branch says, and the branch is what just changed
// underneath it. So the assertion is not only "the image came back" but "the
// history grew", and the commit that deployed the newer image is still on it.
func TestARollbackIsANewCommitAndNotARewind(t *testing.T) {
	l := newLifecycle(t)

	l.deployed(imageOne, `{"image":"`+imageOne+`"}`)
	l.deployed(imageTwo, `{"image":"`+imageTwo+`"}`)
	before := commitsOnMain(t, l.repo)

	code, body := l.rollback("1", accMember)
	if code != http.StatusAccepted {
		t.Fatalf("rollback to 1 = %d: %s", code, body)
	}

	if got := committedWorkload(t, l.repo).Spec.Image; got != imageOne {
		t.Errorf("committed image = %q, want the image deploy 1 ran", got)
	}
	if after := commitsOnMain(t, l.repo); after != before+1 {
		t.Errorf("main has %d commits after a rollback, want %d: a rollback adds a commit, "+
			"it does not remove one", after, before+1)
	}

	// And it is a deploy in its own right: a third record, with the rollback
	// named in it, attributed to whoever asked for it.
	page, err := l.records.History(context.Background(), evidence.Query{
		Ref:   evidence.Ref{TenantID: tenantHome, App: appAPI, Env: envProd},
		Order: evidence.OrderNewest, Limit: 10,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(page.Records) != 3 {
		t.Fatalf("the log has %d records after two deploys and a rollback, want 3", len(page.Records))
	}
	newest := page.Records[0]
	if newest.Seq != 3 {
		t.Errorf("the rollback record is seq %d, want 3", newest.Seq)
	}
	if !strings.Contains(newest.Note, "roll") || !strings.Contains(newest.Note, "deploy 1") {
		t.Errorf("the record does not say it was a rollback: %q", newest.Note)
	}
	if newest.Actor.ID != accMember {
		t.Errorf("the rollback is attributed to %q, want the person who asked", newest.Actor.ID)
	}
}

// The image and only the image. Everything else keeps what is committed now,
// which is a limit of what an evidence record carries rather than a choice —
// see rollbackRoute for what would have to exist to do better.
func TestARollbackRestoresTheImageAndLeavesTheRestAlone(t *testing.T) {
	l := newLifecycle(t)

	l.deployed(imageOne, `{"image":"`+imageOne+`","port":8080}`)
	l.deployed(imageTwo,
		`{"image":"`+imageTwo+`","port":9090,"domain":"api.example.test"}`)

	if code, body := l.rollback("1", accMember); code != http.StatusAccepted {
		t.Fatalf("rollback = %d: %s", code, body)
	}
	got := committedWorkload(t, l.repo)
	if got.Spec.Image != imageOne {
		t.Errorf("Image = %q, want it rolled back", got.Spec.Image)
	}
	if got.Spec.Port != 9090 {
		t.Errorf("Port = %d, want the committed 9090: a rollback restores the image, not the spec",
			got.Spec.Port)
	}
	if got.Spec.Domain != "api.example.test" {
		t.Errorf("Domain = %q, want the committed one", got.Spec.Domain)
	}
}

// A deploy that admission refused, or that never rolled out, is not something
// to restore: rolling back to it would put an image nobody saw run into the
// cluster on the strength of a record that says it did not.
func TestARollbackRefusesADeployThatNeverRan(t *testing.T) {
	for _, state := range []evidence.State{evidence.StateRejected, evidence.StateFailed} {
		t.Run(string(state), func(t *testing.T) {
			l := newLifecycle(t)
			if code, body := l.deploy(`{"image":"` + imageOne + `"}`); code != http.StatusAccepted {
				t.Fatalf("deploy = %d: %s", code, body)
			}
			ref := evidence.Ref{TenantID: tenantHome, App: appAPI, Env: envProd}
			page, err := l.records.History(context.Background(), evidence.Query{
				Ref: ref, Order: evidence.OrderNewest, Limit: 1,
			})
			if err != nil || len(page.Records) == 0 {
				t.Fatalf("reading the record back: %v", err)
			}
			rec := page.Records[0]
			if _, err := l.records.Transition(context.Background(), rec.ID, evidence.Transition{
				From: []evidence.State{rec.State}, To: state,
				At: time.Now().UTC(), Reason: "for the test",
			}); err != nil {
				t.Fatalf("Transition: %v", err)
			}

			code, body := l.rollback("1", accMember)
			if code != http.StatusConflict {
				t.Fatalf("rollback to a %s deploy = %d, want 409: %s", state, code, body)
			}
			if !strings.Contains(body, string(state)) {
				t.Errorf("the refusal does not say what the record holds: %s", body)
			}
		})
	}
}

// A number nobody deployed is a 404, and it is answered without reading to the
// beginning of the log: history is newest-first and sequence numbers are
// gapless, so the first record older than the target proves there is no target.
func TestARollbackToANumberThatWasNeverDeployed(t *testing.T) {
	l := newLifecycle(t)
	l.deployed(imageOne, `{"image":"`+imageOne+`"}`)
	if code, _ := l.rollback("7", accMember); code != http.StatusNotFound {
		t.Errorf("rollback to a deploy that does not exist = %d, want 404", code)
	}
	// And an app with no deploys at all, which reaches an empty first page
	// rather than the comparison above.
	empty := newLifecycle(t)
	if code, _ := empty.rollback("1", accMember); code != http.StatusNotFound {
		t.Errorf("rollback with nothing deployed = %d, want 404", code)
	}
}

func TestARollbackNeedsANumber(t *testing.T) {
	l := newLifecycle(t)
	for _, seq := range []string{"0", "-1", "latest", "1.5", "9999999999999999999999"} {
		if code, _ := l.rollback(seq, accMember); code != http.StatusBadRequest {
			t.Errorf("rollback to %q = %d, want 400", seq, code)
		}
	}
}

// Rolling back to what is already running still makes a commit. This case
// records that rather than blessing it, because the code one layer down claims
// otherwise.
//
// gitwrite has an ErrNoChange for "the manifests are already what is committed"
// and server/deploy.go maps it to a 409 with "this is already what is
// committed". It cannot fire through either path: manifest.Render stamps a
// fresh damga.co/rollout annotation on every render, so the rendered bytes
// always differ, the worktree is never clean, and the branch is unreachable.
// Measured here — this is a rollback to the deploy that is already running and
// it answers 202 with a commit behind it.
//
// Left alone rather than worked around. The rollout id has to be inside the
// manifest before the commit, because the observer finds a record by reading it
// off the live object; suppressing it is a change to how a deploy is identified,
// not a change to this endpoint. The mapping stays in commitChange because it
// costs nothing and is correct the day that changes.
func TestARollbackToWhatIsAlreadyRunningStillCommits(t *testing.T) {
	l := newLifecycle(t)
	l.deployed(imageOne, `{"image":"`+imageOne+`"}`)
	before := commitsOnMain(t, l.repo)

	code, body := l.rollback("1", accMember)
	if code != http.StatusAccepted {
		t.Fatalf("rollback to the running deploy = %d: %s", code, body)
	}
	if after := commitsOnMain(t, l.repo); after != before+1 {
		t.Errorf("main has %d commits, want %d: if this ever stops committing, "+
			"ErrNoChange has become reachable and deploy.go's 409 is live", after, before+1)
	}
	if got := committedWorkload(t, l.repo).Spec.Image; got != imageOne {
		t.Errorf("Image = %q, want it unchanged", got)
	}
}

// The walk backwards is bounded, because it runs inside an HTTP request and
// evidence.Query cannot ask for one sequence number.
//
// The refusal is deliberately not a 404: the deploy exists, and telling the
// caller it does not would send them looking for a typo instead of at the
// limitation. It answers 501 for the same reason the restart does — this
// installation cannot, and giving evidence.Query a Seq filter is what fixes it.
func TestARollbackFurtherBackThanTheScanSaysSoRatherThanLying(t *testing.T) {
	l := newLifecycle(t)
	ctx := context.Background()
	ref := evidence.Ref{TenantID: tenantHome, App: appAPI, Env: envProd}

	// Appended straight to the store rather than deployed: what is under test
	// is the walk, and 2001 clones would be a slow way to say so.
	for i := range maxRollbackScan + 1 {
		if _, err := l.records.Append(ctx, evidence.Record{
			IdempotencyKey: "deep:" + strconv.Itoa(i),
			Ref:            ref,
			Actor:          evidence.Actor{ID: accMember, Kind: kindUser},
			Image:          evidence.Image{RequestedRef: imageOne},
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	code, body := l.rollback("1", accMember)
	if code != http.StatusNotImplemented {
		t.Fatalf("rollback past the scan = %d, want 501: %s", code, body)
	}
	if !strings.Contains(body, strconv.Itoa(maxRollbackScan)) {
		t.Errorf("the refusal does not say how far back it can reach: %s", body)
	}

	// And one inside the bound is still reachable, so the bound is a bound and
	// not a broken walk.
	if code, body := l.rollback(strconv.Itoa(maxRollbackScan+1), accMember); code != http.StatusAccepted {
		t.Errorf("rollback inside the scan = %d, want 202: %s", code, body)
	}
}

func TestARollbackRefusesAViewer(t *testing.T) {
	l := newLifecycle(t)
	l.deployed(imageOne, `{"image":"`+imageOne+`"}`)
	if code, _ := l.rollback("1", accViewer); code != http.StatusForbidden {
		t.Errorf("a viewer rolled an app back: %d", code)
	}
}

// ---------------------------------------------------------------- scale

func TestScaleCommitsTheReplicaCountAndNothingElse(t *testing.T) {
	l := newLifecycle(t)
	if code, body := l.deploy(`{"image":"` + imageOne + `","port":8080}`); code != http.StatusAccepted {
		t.Fatalf("deploy = %d: %s", code, body)
	}

	code, body := l.scale(accMember, `{"replicas":3}`)
	if code != http.StatusAccepted {
		t.Fatalf("scale = %d: %s", code, body)
	}
	got := committedWorkload(t, l.repo)
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 3 {
		t.Errorf("Replicas = %v, want 3", got.Spec.Replicas)
	}
	if got.Spec.Image != imageOne {
		t.Errorf("Image = %q: a scale must not change what runs, only how much of it", got.Spec.Image)
	}
	if got.Spec.Port != 8080 {
		t.Errorf("Port = %d, want it untouched", got.Spec.Port)
	}
}

// The CRD refuses replicas and autoscale together. Without this check the
// commit is pushed, and then the API server rejects it on every sync — a scale
// that reported success and left the app unable to sync at all.
func TestScaleRefusesAnAutoscaledWorkload(t *testing.T) {
	l := newLifecycle(t)
	seedWorkload(t, l.repo, platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: appAPI, Namespace: nsHomeProd},
		Spec: platformv1alpha1.WorkloadSpec{
			Image:     imageOne,
			Autoscale: &platformv1alpha1.Autoscale{MinReplicas: 1, MaxReplicas: 5},
		},
	})

	code, body := l.scale(accMember, `{"replicas":3}`)
	if code != http.StatusConflict {
		t.Fatalf("scaling an autoscaled workload = %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "otomatik ölçekleniyor") {
		t.Errorf("the refusal does not say why: %s", body)
	}
	// Nothing was committed: the manifest still autoscales.
	if got := committedWorkload(t, l.repo); got.Spec.Replicas != nil {
		t.Errorf("the refused scale was committed anyway: replicas = %v", got.Spec.Replicas)
	}
}

// A workload with a volume is pinned to one replica by the operator, which
// overrides the field rather than refusing it. Nothing would have rejected this
// commit; the count would simply never have taken effect.
func TestScaleRefusesAWorkloadWithAVolume(t *testing.T) {
	l := newLifecycle(t)
	seedWorkload(t, l.repo, platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: appAPI, Namespace: nsHomeProd},
		Spec: platformv1alpha1.WorkloadSpec{
			Image: imageOne,
			Volumes: []platformv1alpha1.Volume{{
				Name: "data", Path: "/data", Size: resource.MustParse("1Gi"),
			}},
		},
	})

	code, body := l.scale(accMember, `{"replicas":3}`)
	if code != http.StatusConflict {
		t.Fatalf("scaling a workload with a volume = %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "bir replika çalıştırıyor") {
		t.Errorf("the refusal does not say why: %s", body)
	}
	if got := committedWorkload(t, l.repo); got.Spec.Replicas != nil {
		t.Errorf("the refused scale was committed anyway: replicas = %v", got.Spec.Replicas)
	}
}

func TestScaleNeedsACountAndRefusesZero(t *testing.T) {
	l := newLifecycle(t)
	if code, body := l.deploy(`{"image":"` + imageOne + `"}`); code != http.StatusAccepted {
		t.Fatalf("deploy = %d: %s", code, body)
	}
	for _, c := range []struct{ name, body string }{
		{"no count at all", `{}`},
		// Not read as "not sent". "Take the app down" is a thing somebody will
		// type, and the type has no way to express a stopped app.
		{"zero", `{"replicas":0}`},
		{"negative", `{"replicas":-2}`},
		{"not JSON", `{`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if code, body := l.scale(accMember, c.body); code != http.StatusBadRequest {
				t.Errorf("scale %s = %d, want 400: %s", c.name, code, body)
			}
		})
	}
}

// An app created and never deployed has a placement and no manifest. Scaling it
// would render a Workload out of nothing and fail on the missing image, with a
// message about manifests rather than about this app.
func TestScaleBeforeAnythingIsDeployed(t *testing.T) {
	l := newLifecycle(t)
	code, body := l.scale(accMember, `{"replicas":2}`)
	if code != http.StatusConflict {
		t.Fatalf("scaling before the first deploy = %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "hiçbir şey deploy edilmedi") {
		t.Errorf("the refusal does not say what is missing: %s", body)
	}
}

func TestScaleRefusesAViewer(t *testing.T) {
	l := newLifecycle(t)
	if code, body := l.deploy(`{"image":"` + imageOne + `"}`); code != http.StatusAccepted {
		t.Fatalf("deploy = %d: %s", code, body)
	}
	if code, _ := l.scale(accViewer, `{"replicas":3}`); code != http.StatusForbidden {
		t.Errorf("a viewer scaled an app: %d", code)
	}
}

// ---------------------------------------------------------------- restart

// The endpoint cannot work, and it says which kind of cannot.
//
// A restart is a change to the pod template, the only write path is git, and
// nothing a Workload carries reaches the pod template — read in
// internal/controller/resources.go, where the Deployment's ObjectMeta takes the
// platform's annotations and the template's ObjectMeta takes labels and nothing
// else. Answering anything other than 501 would mean doing something adjacent
// and calling it a restart.
func TestRestartSaysItCannotAndSaysWhy(t *testing.T) {
	l := newLifecycle(t)
	if code, body := l.deploy(`{"image":"` + imageOne + `"}`); code != http.StatusAccepted {
		t.Fatalf("deploy = %d: %s", code, body)
	}
	before := commitsOnMain(t, l.repo)

	code, body := l.restart(accMember)
	if code != http.StatusNotImplemented {
		t.Fatalf("restart = %d, want 501: %s", code, body)
	}
	if !strings.Contains(body, "pod şablonu") {
		t.Errorf("the refusal does not say what is missing: %s", body)
	}
	// And it did nothing on the way: a refusal that commits is worse than one
	// that does not, because the history then claims something happened.
	if after := commitsOnMain(t, l.repo); after != before {
		t.Errorf("a refused restart committed: %d commits, was %d", after, before)
	}
}

// "There is no such app here" and "this platform cannot restart anything" are
// different answers, and without the placement lookup every typo in an app name
// reads as a missing feature.
func TestRestartOfAnAppThatIsNotPlacedIs404(t *testing.T) {
	h := newHarness(t)
	code, body := (&lifecycle{harness: h}).restart(accMember)
	if code != http.StatusNotFound {
		t.Fatalf("restart of an app with no placement = %d, want 404: %s", code, body)
	}
}

func TestRestartRefusesAViewer(t *testing.T) {
	l := newLifecycle(t)
	if code, _ := l.restart(accViewer); code != http.StatusForbidden {
		t.Errorf("a viewer restarted an app: %d", code)
	}
}

// A deploy that nothing observed can still be rolled back to.
//
// This test asserted the opposite when it was written, and the reason it
// changed is worth keeping. internal/gitwrite opened a record with the ref, the
// actor and the source and left evidence.Image empty; the observer filled it in
// later from the live Deployment. So on an install running with ObserveDeploys
// off — a legitimate way to run a control plane that does not live in the
// target cluster — every record carried an empty image and there was nothing to
// roll back to. Refusing was the only honest answer available.
//
// It was the wrong thing to fix here. The record should have carried what was
// asked for from the moment it was asked, and it does now: the write path
// records the image, and the observer's image remains a separate fact about
// what ended up running. The refusal this replaced is still reachable for a
// record that genuinely has no image, and the case below asserts that too.
func TestARollbackReachesADeployNothingObserved(t *testing.T) {
	l := newLifecycle(t)
	if code, body := l.deploy(`{"image":"` + imageOne + `"}`); code != http.StatusAccepted {
		t.Fatalf("deploy = %d: %s", code, body)
	}
	if code, body := l.deploy(`{"image":"` + imageTwo + `"}`); code != http.StatusAccepted {
		t.Fatalf("second deploy = %d: %s", code, body)
	}
	before := commitsOnMain(t, l.repo)

	code, body := l.rollback("1", accMember)
	if code != http.StatusAccepted {
		t.Fatalf("rollback to an unobserved deploy = %d, want 202: %s\n"+
			"the write path records the image now, so there is something to roll back to",
			code, body)
	}
	if after := commitsOnMain(t, l.repo); after != before+1 {
		t.Errorf("a rollback did not commit: %d commits, was %d", after, before)
	}
}

// The identity that writes the fixture commits these suites start from. Named
// once because two files seed repositories the same way, and a reader comparing
// them should be comparing one value to itself.
const (
	seedName  = "seed"
	seedEmail = "seed@example.test"
)
