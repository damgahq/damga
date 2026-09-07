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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/internal/manifest"
)

const (
	// The value every case types. Long enough that finding it in a file is
	// proof rather than a coincidence.
	settingsSecretValue = "s3cr3t-value-nothing-may-commit"
	settingsSecretKey   = "SMTP_PASSWORD"

	// Shared with the cases in lifecycle_internal_test.go, which use the same
	// two values for the same reasons.
	testAppDomain = "api.example.test"
	volumeName    = "data"
)

// recordingSecrets is the cluster, minus the cluster.
//
// It records rather than stores, because what these cases are about is which
// call was made with which keys — a value kept, a value replaced, a key removed
// after the commit and not before.
type recordingSecrets struct {
	calls []secretCall
	err   error
}

type secretCall struct {
	namespace, name string
	set             map[string]string
	remove          []string
}

func (s *recordingSecrets) Put(
	_ context.Context, namespace, name string, set map[string]string, remove []string,
) error {
	s.calls = append(s.calls, secretCall{namespace: namespace, name: name, set: set, remove: remove})
	return s.err
}

// The last write wins, which is what "the value now" means. Reading the first
// one made a case pass for the wrong reason: it asserted the value from the
// save before the one it was about.
func (s *recordingSecrets) wrote(key string) (string, bool) {
	for _, c := range slices.Backward(s.calls) {
		if v, ok := c.set[key]; ok {
			return v, true
		}
	}
	return "", false
}

func (s *recordingSecrets) removed(key string) bool {
	for _, c := range s.calls {
		if slices.Contains(c.remove, key) {
			return true
		}
	}
	return false
}

// settings drives the two routes through a mux, so the path values the guard
// reads are set the way the router sets them.
func (l *lifecycle) settings(method, account, body string) (int, string) {
	l.t.Helper()
	mux := http.NewServeMux()
	handler := settingsRoute
	if method == http.MethodPut {
		handler = updateSettingsRoute
	}
	suffix := settingsPath
	mux.Handle(method+" "+tenantScope+suffix, handler(l.guard, l.stores))

	target := strings.NewReplacer(
		"{tenant}", tenantHome, "{app}", appAPI, "{env}", envProd,
	).Replace(tenantScope + suffix)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = testHost
	if account != "" {
		req.AddCookie(l.cookies[account])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// configured is an app that has been deployed, which is the state every case
// below starts from: settings are settings *of* something.
func configured(t *testing.T) (*lifecycle, *recordingSecrets) {
	t.Helper()
	l := newLifecycle(t)
	secrets := &recordingSecrets{}
	l.stores.secrets = secrets
	l.deployed(imageOne, `{"image":"`+imageOne+`"}`)
	return l, secrets
}

func decodeSettings(t *testing.T, body string) wireSettings {
	t.Helper()
	var out struct {
		wireSettings
		Settings *wireSettings `json:"settings"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, body)
	}
	if out.Settings != nil {
		return *out.Settings
	}
	return out.wireSettings
}

func envOf(s wireSettings, key string) (wireEnvVar, bool) {
	for _, v := range s.Env {
		if v.Key == key {
			return v, true
		}
	}
	return wireEnvVar{}, false
}

// The whole reason this endpoint exists, asserted at the only place it can be:
// the value a user typed reaches the cluster and does not reach git.
//
// n8n does not install from the catalogue because its template shares one token
// between two variables and nothing let a person supply it. Coolify's answer is
// a text box whose value lives in its own database, encrypted, where git cannot
// show even that the variable exists. This is the other half of that trade —
// git says which variables there are and where each value lives, and the value
// itself is somewhere a commit never goes.
func TestASecretValueReachesTheClusterAndNeverTheCommit(t *testing.T) {
	l, secrets := configured(t)

	code, body := l.settings(http.MethodPut, accMember, `{
		"env": [
			{"key":"LOG_LEVEL","value":"debug","runtime":true},
			{"key":"`+settingsSecretKey+`","value":"`+settingsSecretValue+`","secret":true,"runtime":true}
		],
		"health": {}, "resources": {}
	}`)
	if code != http.StatusAccepted {
		t.Fatalf("PUT = %d: %s", code, body)
	}

	// In the cluster.
	if got, ok := secrets.wrote(settingsSecretKey); !ok || got != settingsSecretValue {
		t.Errorf("the Secret was written with %q (present: %v)", got, ok)
	}
	if c := secrets.calls[0]; c.namespace != nsHomeProd || c.name != manifest.UserSecretName(appAPI) {
		t.Errorf("the value was written to %s/%s", c.namespace, c.name)
	}

	// And nowhere in git. Every committed file in the whole repository, not
	// just the manifest and not just this app's directory: a value that leaked
	// into the fence, into a sibling, or into somebody else's tree would still
	// be a value in a repository, and that is the failure this arrangement
	// exists to avoid.
	//
	// The first version of this used committedNames, which filters to the
	// catalogue's install directory — so it walked an empty map and asserted
	// nothing at all. A mutation that committed the value as a literal env var
	// went straight past it.
	for name, file := range everyCommittedFile(t, l.repo) {
		if strings.Contains(string(file), settingsSecretValue) {
			t.Fatalf("the secret value is committed in %s; git carries the name and the "+
				"reference and must never carry the value", name)
		}
	}

	// What git does carry: the literal, and the name of the secret.
	app := committedWorkload(t, l.repo)
	if len(app.Spec.Env) != 1 || app.Spec.Env[0].Name != "LOG_LEVEL" || app.Spec.Env[0].Value != "debug" {
		t.Errorf("committed env = %v", app.Spec.Env)
	}
	if !slices.Contains(app.Spec.UserSecrets, settingsSecretKey) {
		t.Errorf("committed userSecrets = %v, so nothing says the variable exists", app.Spec.UserSecrets)
	}
}

// And the read side of the same rule.
func TestTheAnswerCarriesTheSecretsNameAndNotItsValue(t *testing.T) {
	l, _ := configured(t)
	if code, body := l.settings(http.MethodPut, accMember, `{
		"env": [{"key":"`+settingsSecretKey+`","value":"`+settingsSecretValue+`","secret":true,"runtime":true}],
		"health": {}, "resources": {}
	}`); code != http.StatusAccepted {
		t.Fatalf("PUT = %d: %s", code, body)
	}

	code, body := l.settings(http.MethodGet, accViewer, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d: %s", code, body)
	}
	if strings.Contains(body, settingsSecretValue) {
		t.Fatalf("the GET answer carries the secret value:\n%s", body)
	}
	got, ok := envOf(decodeSettings(t, body), settingsSecretKey)
	if !ok {
		t.Fatalf("the secret is not in the answer at all:\n%s", body)
	}
	if !got.Secret || got.Value != nil {
		t.Errorf("the secret came back as %+v; a value of \"\" and a value that is not shown "+
			"are different facts and the panel draws them differently", got)
	}
	if got.SecretRef == nil || got.SecretRef.Name != manifest.UserSecretName(appAPI) ||
		got.SecretRef.Key != settingsSecretKey {
		t.Errorf("secretRef = %+v: which Secret a variable reads is the thing git can show "+
			"and a platform holding values in its own database cannot", got.SecretRef)
	}
	// The sentence about what this costs, from the server rather than invented
	// by the page — so the two halves cannot end up saying different things.
	if !strings.Contains(body, "no rollback") {
		t.Error("the answer does not carry the note about a value git does not have")
	}
}

// Saving a form without retyping every password on it.
func TestASecretWithNoValueIsKeptAndNotCleared(t *testing.T) {
	l, secrets := configured(t)
	set := `{"env":[{"key":"` + settingsSecretKey + `","value":"` + settingsSecretValue +
		`","secret":true,"runtime":true}],"health":{},"resources":{}}`
	if code, body := l.settings(http.MethodPut, accMember, set); code != http.StatusAccepted {
		t.Fatalf("first PUT = %d: %s", code, body)
	}
	secrets.calls = nil

	// The same list, with no value on the secret: what a form sends when
	// nobody retyped it.
	code, body := l.settings(http.MethodPut, accMember, `{
		"env": [
			{"key":"`+settingsSecretKey+`","secret":true,"runtime":true},
			{"key":"LOG_LEVEL","value":"info","runtime":true}
		], "health": {}, "resources": {}
	}`)
	if code != http.StatusAccepted {
		t.Fatalf("second PUT = %d: %s", code, body)
	}
	if _, wrote := secrets.wrote(settingsSecretKey); wrote {
		t.Error("the secret was written again on a save that did not carry a value")
	}
	if secrets.removed(settingsSecretKey) {
		t.Fatal("a save that did not retype the password deleted it, which is the worst " +
			"thing this endpoint can do")
	}
	if app := committedWorkload(t, l.repo); !slices.Contains(app.Spec.UserSecrets, settingsSecretKey) {
		t.Errorf("the manifest stopped naming the secret: %v", app.Spec.UserSecrets)
	}
}

// Removal is omission, and the order of the two writes is the subject.
func TestRemovingAVariableTakesItsValueAfterTheCommitAndNotBefore(t *testing.T) {
	l, secrets := configured(t)
	if code, body := l.settings(http.MethodPut, accMember, `{
		"env": [{"key":"`+settingsSecretKey+`","value":"`+settingsSecretValue+`","secret":true,"runtime":true}],
		"health": {}, "resources": {}
	}`); code != http.StatusAccepted {
		t.Fatalf("PUT = %d: %s", code, body)
	}
	before := commitsOnMain(t, l.repo)
	secrets.calls = nil

	if code, body := l.settings(http.MethodPut, accMember,
		`{"env": [], "health": {}, "resources": {}}`); code != http.StatusAccepted {
		t.Fatalf("PUT = %d: %s", code, body)
	}
	if !secrets.removed(settingsSecretKey) {
		t.Error("the value is still in the cluster after the variable was removed")
	}
	if app := committedWorkload(t, l.repo); len(app.Spec.UserSecrets) != 0 {
		t.Errorf("the manifest still names %v", app.Spec.UserSecrets)
	}
	if commitsOnMain(t, l.repo) != before+1 {
		t.Error("the removal did not produce a commit")
	}
}

// The refusals, each one a sentence about one field.
func TestTheRefusalsSayWhichFieldAndWhy(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			"an empty value on a secret is not a way to clear it",
			`{"env":[{"key":"A","value":"","secret":true,"runtime":true}],"health":{},"resources":{}}`,
			"leave A out of the list",
		},
		{
			"a build-time secret has nowhere to live",
			`{"env":[{"key":"A","value":"x","secret":true,"build":true}],"health":{},"resources":{}}`,
			"builds run in their own namespace",
		},
		{
			"a variable that is neither is delivered nowhere",
			`{"env":[{"key":"A","value":"x"}],"health":{},"resources":{}}`,
			"neither a build-time nor a run-time variable",
		},
		{
			"a name envFrom would silently skip",
			`{"env":[{"key":"a.b","value":"x","runtime":true}],"health":{},"resources":{}}`,
			"not a usable environment variable name",
		},
		{
			"one variable, one value",
			`{"env":[{"key":"A","value":"1","runtime":true},{"key":"A","value":"2","runtime":true}],
			  "health":{},"resources":{}}`,
			"listed twice",
		},
		{
			"a literal with no value at all",
			`{"env":[{"key":"A","runtime":true}],"health":{},"resources":{}}`,
			"send one, and send",
		},
		{
			"a probe that is still running when the next one is due",
			`{"env":[],"health":{"intervalSeconds":5,"timeoutSeconds":30},"resources":{}}`,
			"timeout cannot be longer than the interval",
		},
		{
			"a memory limit below the request, which the kubelet refuses",
			`{"env":[],"health":{},"resources":{"memoryRequest":"512Mi","memoryLimit":"128Mi"}}`,
			"below the memory request",
		},
		{
			"a quantity that is not one",
			`{"env":[],"health":{},"resources":{"cpuRequest":"a lot"}}`,
			"is not a quantity",
		},
		{
			"a secret that was never set cannot be kept",
			`{"env":[{"key":"NEW_ONE","secret":true,"runtime":true}],"health":{},"resources":{}}`,
			"a secret that was never set cannot be kept",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, secrets := configured(t)
			before := commitsOnMain(t, l.repo)
			code, body := l.settings(http.MethodPut, accMember, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400: %s", code, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("the refusal is %s\nand does not say %q", body, tc.want)
			}
			// Nothing half-applied. A refusal that had already written a value
			// or a commit would leave the app in a state nobody asked for.
			if len(secrets.calls) != 0 {
				t.Errorf("a refused request wrote %d times to the cluster", len(secrets.calls))
			}
			if commitsOnMain(t, l.repo) != before {
				t.Error("a refused request committed")
			}
		})
	}
}

// A change git cannot carry still gets a commit, and the answer says what that
// commit does and does not contain.
//
// This is not the shape the endpoint was designed for and the design was wrong
// about its own layer: manifest.Render stamps a fresh damga.co/rollout
// annotation on every render, so the rendered bytes always differ and
// gitwrite's ErrNoChange cannot fire through any server path — measured and
// written down in TestARollbackToWhatIsAlreadyRunningStillCommits before this
// endpoint existed.
//
// What comes out of that is better than what was planned. A secret value that
// changed produces a commit whose diff holds no value, which is exactly the
// trade docs/KARARLAR.md made: git gets who and when, and never what.
func TestChangingOnlyASecretValueIsCommittedWithoutTheValue(t *testing.T) {
	l, secrets := configured(t)
	first := `{"env":[{"key":"` + settingsSecretKey + `","value":"one","secret":true,"runtime":true}],
	           "health":{},"resources":{}}`
	if code, body := l.settings(http.MethodPut, accMember, first); code != http.StatusAccepted {
		t.Fatalf("first PUT = %d: %s", code, body)
	}
	before := commitsOnMain(t, l.repo)

	code, body := l.settings(http.MethodPut, accMember,
		`{"env":[{"key":"`+settingsSecretKey+`","value":"`+settingsSecretValue+`","secret":true,"runtime":true}],
		  "health":{},"resources":{}}`)
	if code != http.StatusAccepted {
		t.Fatalf("PUT = %d: %s", code, body)
	}
	if got, _ := secrets.wrote(settingsSecretKey); got != settingsSecretValue {
		t.Errorf("the new value was not written: %q", got)
	}
	if after := commitsOnMain(t, l.repo); after != before+1 {
		t.Errorf("main has %d commits, want %d", after, before+1)
	}
	for name, file := range everyCommittedFile(t, l.repo) {
		if strings.Contains(string(file), settingsSecretValue) {
			t.Fatalf("%s carries the new secret value", name)
		}
	}
	// And the answer does not let "committed" be read as "recoverable".
	if !strings.Contains(body, "not in the commit") {
		t.Errorf("the answer does not say the value is outside the commit:\n%s", body)
	}
}

// An install with no cluster can still set a literal, and says which half is
// missing rather than committing a manifest naming a Secret nobody wrote.
func TestWithNoPlaceToPutASecretTheAnswerSaysSo(t *testing.T) {
	l, _ := configured(t)
	l.stores.secrets = nil

	code, body := l.settings(http.MethodPut, accMember,
		`{"env":[{"key":"`+settingsSecretKey+`","value":"x","secret":true,"runtime":true}],
		  "health":{},"resources":{}}`)
	if code != http.StatusConflict {
		t.Fatalf("PUT = %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "cannot hold a secret value") {
		t.Errorf("the refusal is %s", body)
	}

	if code, body := l.settings(http.MethodPut, accMember,
		`{"env":[{"key":"LOG_LEVEL","value":"debug","runtime":true}],"health":{},"resources":{}}`,
	); code != http.StatusAccepted {
		t.Fatalf("a literal-only save on the same install = %d: %s", code, body)
	}
}

// The four fields that were missing, and the reason the first one was the most
// expensive: a probe pointed at a port the application does not serve.
func TestHealthSettingsReachTheCommittedManifest(t *testing.T) {
	l, _ := configured(t)
	code, body := l.settings(http.MethodPut, accMember, `{
		"env": [],
		"health": {"livenessPath":"/live","readinessPath":"/ready","port":9000,
		           "intervalSeconds":20,"timeoutSeconds":5,"failureThreshold":6},
		"resources": {"cpuRequest":"250m","memoryRequest":"256Mi","memoryLimit":"1Gi"}
	}`)
	if code != http.StatusAccepted {
		t.Fatalf("PUT = %d: %s", code, body)
	}
	app := committedWorkload(t, l.repo)
	h := app.Spec.Health
	if h.Port != 9000 || h.IntervalSeconds != 20 || h.TimeoutSeconds != 5 || h.FailureThreshold != 6 {
		t.Errorf("committed health = %+v", h)
	}
	if h.LivenessPath != "/live" || h.ReadinessPath != "/ready" {
		t.Errorf("committed paths = %s %s", h.LivenessPath, h.ReadinessPath)
	}
	if got := app.Spec.Resources.MemoryLimit.String(); got != "1Gi" {
		t.Errorf("committed memory limit = %s", got)
	}
	// And they come back.
	_, read := l.settings(http.MethodGet, accViewer, "")
	if s := decodeSettings(t, read); s.Health.Port != 9000 || s.Resources.CPURequest != "250m" {
		t.Errorf("the answer lost them: %+v %+v", s.Health, s.Resources)
	}
}

// A build-time variable is recorded, and the answer says it is delivered
// nowhere. Both halves matter: dropping it would lose a setting somebody made,
// and accepting it silently would be the "stored and does nothing" this page
// exists to expose.
func TestABuildTimeVariableIsKeptAndDeclaredUnconsumed(t *testing.T) {
	l, _ := configured(t)
	code, body := l.settings(http.MethodPut, accMember,
		`{"env":[{"key":"NODE_ENV","value":"production","build":true,"runtime":true}],
		  "health":{},"resources":{}}`)
	if code != http.StatusAccepted {
		t.Fatalf("PUT = %d: %s", code, body)
	}
	app := committedWorkload(t, l.repo)
	if len(app.Spec.BuildEnv) != 1 || app.Spec.BuildEnv[0].Name != "NODE_ENV" {
		t.Errorf("buildEnv = %v", app.Spec.BuildEnv)
	}
	if len(app.Spec.Env) != 1 {
		t.Errorf("a variable that is both build-time and run-time lost its run-time half: %v",
			app.Spec.Env)
	}
	// The warning that used to be here said nothing consumed a build-time
	// variable, which was true until BuildSpec grew an Env. What is left to
	// warn about is the thing somebody gets wrong next: looking for it in the
	// container that is already running.
	if !strings.Contains(body, "not the container that is running now") {
		t.Errorf("the answer does not say when a build-time variable takes effect:\n%s", body)
	}

	// One row, both boxes ticked. Two rows with the same name is a screen
	// nobody can act on.
	_, read := l.settings(http.MethodGet, accViewer, "")
	s := decodeSettings(t, read)
	if len(s.Env) != 1 {
		t.Fatalf("the same variable came back %d times: %+v", len(s.Env), s.Env)
	}
	if !s.Env[0].Build || !s.Env[0].Runtime {
		t.Errorf("the flags did not survive the round trip: %+v", s.Env[0])
	}
}

// Settings are settings of something. An app registered and never deployed has
// no manifest, and rendering one out of nothing would fail deeper down with a
// message about manifests rather than about this app.
func TestConfiguringSomethingThatWasNeverDeployedSaysSo(t *testing.T) {
	l := newLifecycle(t)
	l.stores.secrets = &recordingSecrets{}

	code, body := l.settings(http.MethodPut, accMember,
		`{"env":[{"key":"A","value":"1","runtime":true}],"health":{},"resources":{}}`)
	if code != http.StatusConflict {
		t.Fatalf("PUT = %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "nothing is deployed here yet") {
		t.Errorf("the refusal is %s", body)
	}

	// The read is not an error, though: it is the page somebody is about to
	// use, and an empty list is what it should show.
	code, body = l.settings(http.MethodGet, accMember, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d: %s", code, body)
	}
	if s := decodeSettings(t, body); len(s.Env) != 0 {
		t.Errorf("an app with no manifest reported settings: %+v", s.Env)
	}
}

// A viewer may look and may not change. The route table's own test proves every
// route refuses another tenant; this is about the two roles inside one.
func TestAViewerMayReadSettingsAndNotWriteThem(t *testing.T) {
	l, _ := configured(t)
	if code, body := l.settings(http.MethodGet, accViewer, ""); code != http.StatusOK {
		t.Fatalf("GET as a viewer = %d: %s", code, body)
	}
	code, body := l.settings(http.MethodPut, accViewer,
		`{"env":[{"key":"A","value":"1","runtime":true}],"health":{},"resources":{}}`)
	if code != http.StatusForbidden {
		t.Fatalf("PUT as a viewer = %d, want 403: %s", code, body)
	}
}

// The manifest keeps everything this endpoint is not about.
func TestASettingsChangeLeavesTheImageAndItsSiblingsAlone(t *testing.T) {
	l, _ := configured(t)
	seedWorkload(t, l.repo, platformv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: appAPI, Namespace: nsHomeProd},
		Spec: platformv1alpha1.WorkloadSpec{
			Image: imageTwo, Port: 3000, Domain: testAppDomain,
			Volumes: []platformv1alpha1.Volume{{
				Name: volumeName, Path: "/data", Size: resource.MustParse("1Gi"),
			}},
		},
	})

	if code, body := l.settings(http.MethodPut, accMember,
		`{"env":[{"key":"A","value":"1","runtime":true}],"health":{},"resources":{}}`,
	); code != http.StatusAccepted {
		t.Fatalf("PUT = %d: %s", code, body)
	}
	app := committedWorkload(t, l.repo)
	if app.Spec.Image != imageTwo {
		t.Errorf("the image changed to %s; a settings change has no opinion about it", app.Spec.Image)
	}
	if app.Spec.Domain != "api.example.test" || app.Spec.Port != 3000 {
		t.Errorf("the settings write dropped %s:%d", app.Spec.Domain, app.Spec.Port)
	}
	if len(app.Spec.Volumes) != 1 {
		t.Errorf("the volume is gone: %v", app.Spec.Volumes)
	}
}

// everyCommittedFile is the whole tree at main, which is what "nowhere in git"
// has to mean.
//
// Its own helper rather than the catalogue's committedNames, which filters to
// one directory: a scan for a value that must not exist anywhere cannot be
// scoped to the place it was expected not to be.
func everyCommittedFile(t *testing.T, repoPath string) map[string][]byte {
	t.Helper()
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		t.Fatalf("opening %s: %v", repoPath, err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchMain), true)
	if err != nil {
		t.Fatalf("resolving %s: %v", branchMain, err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("reading the commit: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("reading the tree: %v", err)
	}
	out := map[string][]byte{}
	if err := tree.Files().ForEach(func(f *object.File) error {
		body, err := f.Contents()
		if err != nil {
			return err
		}
		out[f.Name] = []byte(body)
		return nil
	}); err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the repository has no committed files at all, so this scan proves nothing")
	}
	return out
}
