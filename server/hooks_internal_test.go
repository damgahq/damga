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

// In package server rather than server_test for the reason
// routes_internal_test.go gives: this file asserts things about the route table
// and builds a stores value, and exporting either to reach it from outside
// would make the arrangement part of the API.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/internal/buildwatch"
	"github.com/damgahq/damga/placement"
	placementmem "github.com/damgahq/damga/placement/memory"
)

const (
	// The tenant these deliveries resolve to. Its own constant rather than
	// tenantHome, because the repository URL below reads as one story with it
	// and a webhook test whose repository and tenant are unrelated names is
	// harder to follow than it needs to be.
	tenantAcme = "t_acme"

	hookSecret = "a-shared-secret"
	hookRepo   = "https://github.com/acme/api.git"
	hookSHA    = "9b2c1d4e5f60718293a4b5c6d7e8f90112233445"
)

// collectingCreator keeps every Build, which recordingCreator does not: one
// push can legitimately start several, and a creator that remembers the last
// one would make a test for that pass while building only one.
type collectingCreator struct {
	got []*platformv1alpha1.Build
	err error
}

func (c *collectingCreator) CreateBuild(_ context.Context, b *platformv1alpha1.Build) error {
	if c.err != nil {
		return c.err
	}
	if b.Name == "" && b.GenerateName != "" {
		b.Name = b.GenerateName + "xyz12"
	}
	c.got = append(c.got, b)
	return nil
}

// pushBody is a push payload with the five fields this platform reads.
func pushBody(ref, after string) []byte {
	return fmt.Appendf(nil, `{
	  "ref": %q,
	  "after": %q,
	  "deleted": false,
	  "repository": {
	    "clone_url": %q,
	    "html_url": "https://github.com/acme/api",
	    "default_branch": "main",
	    "full_name": "acme/api"
	  }
	}`, ref, after, hookRepo)
}

func signature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// hookEnv is a store with one app whose webhook is registered, plus the handler
// under test.
func hookEnv(t *testing.T, creator BuildCreator, triggers ...placement.Trigger) http.Handler {
	t.Helper()
	ctx := context.Background()
	places := placementmem.New()
	t.Cleanup(func() { _ = places.Close() })

	if len(triggers) == 0 {
		triggers = []placement.Trigger{{
			TenantID: tenantAcme, App: appAPI, Env: envProd,
			Provider: providerGitHub, RepoURL: hookRepo, Secret: hookSecret,
		}}
	}
	for _, tr := range triggers {
		if _, err := places.Put(ctx, placement.Placement{
			TenantID: tr.TenantID, App: tr.App, Env: tr.Env,
			RepoURL:   "https://github.com/damgahq/state-" + tr.TenantID,
			Branch:    "main",
			Path:      "apps/" + tr.App + "/" + tr.Env,
			Namespace: tr.App + "-" + tr.Env + "-" + tr.TenantID,
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := places.SetTrigger(ctx, tr); err != nil {
			t.Fatalf("SetTrigger: %v", err)
		}
	}

	// Through a mux, so {provider} is set the way the real router sets it.
	// Calling the handler directly leaves it empty and every case 404s.
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/hooks/{provider}", hooks(stores{
		placement: places, builds: creator, registry: testRegistry,
	}))
	return mux
}

// deliver posts one delivery and returns what the endpoint answered.
func deliver(h http.Handler, event, sig string, body []byte) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", strings.NewReader(string(body)))
	req.Host = testHost
	if event != "" {
		req.Header.Set(githubEventHeader, event)
	}
	if sig != "" {
		req.Header.Set(githubSignatureHeader, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// The link the chain was missing: a push arrives and a build starts, with
// nobody signed in.
func TestASignedPushStartsABuild(t *testing.T) {
	creator := &collectingCreator{}
	h := hookEnv(t, creator)
	body := pushBody("refs/heads/main", hookSHA)

	code, got := deliver(h, "push", signature(hookSecret, body), body)
	if code != http.StatusAccepted {
		t.Fatalf("a correctly signed push = %d: %s", code, got)
	}
	if len(creator.got) != 1 {
		t.Fatalf("builds started = %d, want 1", len(creator.got))
	}
	build := creator.got[0]
	switch {
	case build.Spec.Revision != hookSHA:
		t.Errorf("revision = %q, want the commit the push named", build.Spec.Revision)
	case build.Spec.Repo != hookRepo:
		t.Errorf("repo = %q, want the repository the push named", build.Spec.Repo)
	case build.Labels["damga.co/tenant"] != tenantAcme:
		t.Errorf("tenant = %q, want the one the trigger resolved to", build.Labels["damga.co/tenant"])
	case build.Labels["damga.co/app"] != appAPI:
		t.Errorf("app = %q", build.Labels["damga.co/app"])
	case build.Namespace != BuildNamespace:
		t.Errorf("namespace = %q, want %q", build.Namespace, BuildNamespace)
	}
	// The image is composed the same way POST /builds composes it, because it
	// is the same function. A second definition here would drift on the first
	// change to either.
	if want := testRegistry + "/" + tenantAcme + "/" + appAPI; build.Spec.Image != want {
		t.Errorf("image = %q, want %q", build.Spec.Image, want)
	}
}

// The identity is the signature. A delivery signed with anything else is
// somebody who found the URL, and the URL is meant to be findable.
func TestAPushSignedWithTheWrongSecretBuildsNothing(t *testing.T) {
	creator := &collectingCreator{}
	h := hookEnv(t, creator)
	body := pushBody("refs/heads/main", hookSHA)

	for _, tc := range []struct{ name, sig string }{
		{"another secret", signature("not-the-secret", body)},
		{"no signature at all", ""},
		{"the right shape, wrong bytes", "sha256=" + strings.Repeat("ab", 32)},
		{"not hex", "sha256=zzzz"},
		{"a sha1 digest, which GitHub still sends beside this one", "sha256=" + strings.Repeat("ab", 20)},
		{"no prefix", hex.EncodeToString(make([]byte, 32))},
		{"a signature over a different body", signature(hookSecret, pushBody("refs/heads/main", strings.Repeat("f", 40)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, got := deliver(h, "push", tc.sig, body)
			if code != http.StatusUnauthorized {
				t.Errorf("%s = %d, want 401: %s", tc.name, code, got)
			}
		})
	}
	if len(creator.got) != 0 {
		t.Errorf("%d builds were started by unverified deliveries", len(creator.got))
	}
}

// A caller who has proved nothing must not be able to learn which repositories
// this install builds. An unknown repository and a bad signature have to be
// indistinguishable — same status, same bytes — or the endpoint becomes a way
// to map a competitor's deployments one name at a time.
func TestAnUnknownRepositoryAndABadSignatureAreIndistinguishable(t *testing.T) {
	h := hookEnv(t, &collectingCreator{})

	known := pushBody("refs/heads/main", hookSHA)
	unknown := []byte(strings.ReplaceAll(string(known), "acme/api", "acme/nobody-has-this"))

	badCode, badBody := deliver(h, "push", signature("wrong", known), known)
	unknownCode, unknownBody := deliver(h, "push", signature(hookSecret, unknown), unknown)

	if badCode != unknownCode {
		t.Errorf("a bad signature answers %d and an unknown repository answers %d", badCode, unknownCode)
	}
	if badBody != unknownBody {
		t.Errorf("the two answers differ:\n bad signature: %s\n unknown repo:  %s", badBody, unknownBody)
	}
	if strings.Contains(unknownBody, "nobody-has-this") || strings.Contains(unknownBody, "acme") {
		t.Errorf("the refusal repeats the repository back: %s", unknownBody)
	}
}

// One repository legitimately feeds several environments, and a person
// configuring them pastes one secret into one webhook. Every environment whose
// secret verifies is built; an environment with a different secret is not.
//
// This also pins the property that makes the comparison safe: verifyGitHub
// checks every candidate rather than returning at the first match, so the
// number of candidates is not learnable from how long the answer took.
func TestOnePushBuildsEveryEnvironmentWhoseSecretVerifies(t *testing.T) {
	creator := &collectingCreator{}
	h := hookEnv(t, creator,
		placement.Trigger{TenantID: tenantAcme, App: appAPI, Env: "dev",
			Provider: providerGitHub, RepoURL: hookRepo, Secret: hookSecret},
		placement.Trigger{TenantID: tenantAcme, App: appAPI, Env: envProd,
			Provider: providerGitHub, RepoURL: hookRepo, Secret: hookSecret},
		// Another tenant building the same public repository, with a secret of
		// their own. They must not be built by somebody else's delivery.
		placement.Trigger{TenantID: tenantOther, App: "fork", Env: envProd,
			Provider: providerGitHub, RepoURL: hookRepo, Secret: "theirs"},
	)
	body := pushBody("refs/heads/main", hookSHA)

	code, got := deliver(h, "push", signature(hookSecret, body), body)
	if code != http.StatusAccepted {
		t.Fatalf("= %d: %s", code, got)
	}
	if len(creator.got) != 2 {
		t.Fatalf("builds = %d, want one per environment that shares the secret", len(creator.got))
	}
	for _, b := range creator.got {
		if b.Labels["damga.co/tenant"] != tenantAcme {
			t.Errorf("built for %q, whose secret did not sign this delivery", b.Labels["damga.co/tenant"])
		}
	}
}

// The delivery a forge makes when the webhook is saved, and the one that tells
// the person who pasted the URL that the URL is right. Answering it only for a
// correct secret would report one mistake as the other.
func TestPingIsAnsweredWithoutASignature(t *testing.T) {
	h := hookEnv(t, &collectingCreator{})

	code, got := deliver(h, "ping", "", []byte(`{"zen":"Design for failure."}`))
	if code != http.StatusOK {
		t.Errorf("ping = %d, want 200: %s", code, got)
	}
	if !strings.Contains(got, "pong") {
		t.Errorf("ping answered %s", got)
	}
}

// A repository's webhook is often configured to send everything. Refusing the
// other event types would fill the forge's delivery log with red that means
// nothing.
func TestAnEventThisPlatformDoesNotActOnIsAccepted(t *testing.T) {
	creator := &collectingCreator{}
	h := hookEnv(t, creator)

	code, got := deliver(h, "issues", "", []byte(`{"action":"opened"}`))
	if code != http.StatusAccepted {
		t.Errorf("an issues event = %d, want 202: %s", code, got)
	}
	if len(creator.got) != 0 {
		t.Error("an issue comment started a build")
	}

	t.Run("no event header at all", func(t *testing.T) {
		if code, _ := deliver(h, "", "", []byte(`{}`)); code != http.StatusBadRequest {
			t.Errorf("= %d, want 400", code)
		}
	})
}

// A deleted branch sends forty zeros where a commit belongs. Building it clones
// a ref that does not exist, and the failure appears minutes later in a build
// log rather than here.
func TestADeletedBranchBuildsNothing(t *testing.T) {
	creator := &collectingCreator{}
	h := hookEnv(t, creator)

	for _, body := range [][]byte{
		pushBody("refs/heads/main", deletedRevision),
		[]byte(strings.Replace(string(pushBody("refs/heads/main", hookSHA)),
			`"deleted": false`, `"deleted": true`, 1)),
	} {
		code, got := deliver(h, "push", signature(hookSecret, body), body)
		if code != http.StatusAccepted {
			t.Errorf("= %d, want 202: %s", code, got)
		}
		if !strings.Contains(got, "ignored") {
			t.Errorf("answered %s, want it to say nothing was built", got)
		}
	}
	if len(creator.got) != 0 {
		t.Errorf("%d builds were started for a branch that no longer exists", len(creator.got))
	}
}

// Only the default branch, and it is a decision rather than a limitation: a
// build spends the cluster's build quota, which one namespace shares between
// every tenant, and nothing in a placement yet says which branch an environment
// tracks.
func TestOnlyThePushToTheDefaultBranchBuilds(t *testing.T) {
	creator := &collectingCreator{}
	h := hookEnv(t, creator)

	for _, ref := range []string{"refs/heads/feature", "refs/tags/v1.0.0", "refs/heads/mainline"} {
		body := pushBody(ref, hookSHA)
		code, got := deliver(h, "push", signature(hookSecret, body), body)
		if code != http.StatusAccepted {
			t.Errorf("%s = %d: %s", ref, code, got)
		}
		if !strings.Contains(got, "default branch") {
			t.Errorf("%s answered %s", ref, got)
		}
	}
	if len(creator.got) != 0 {
		t.Errorf("%d builds were started off the default branch", len(creator.got))
	}

	t.Run("a payload with no default branch builds nothing", func(t *testing.T) {
		// The two wrong answers are not symmetrical: guessing "yes" builds
		// every push to every branch of a payload this platform did not
		// understand.
		body := []byte(strings.Replace(string(pushBody("refs/heads/main", hookSHA)),
			`"default_branch": "main"`, `"default_branch": ""`, 1))
		if code, _ := deliver(h, "push", signature(hookSecret, body), body); code != http.StatusAccepted {
			t.Errorf("= %d", code)
		}
		if len(creator.got) != 0 {
			t.Error("a payload with no default branch was built anyway")
		}
	})
}

// The repository is registered under one spelling and the forge sends another.
// An exact match works for about half of these, and the half that fails looks
// like a delivery that never happened.
func TestTheRepositoryIsMatchedUnderTheSpellingTheForgeSends(t *testing.T) {
	creator := &collectingCreator{}
	// Registered without the ".git" the push payload's clone_url carries.
	h := hookEnv(t, creator, placement.Trigger{
		TenantID: tenantAcme, App: appAPI, Env: envProd,
		Provider: "github", RepoURL: "https://github.com/acme/api", Secret: hookSecret,
	})
	body := pushBody("refs/heads/main", hookSHA)

	if code, got := deliver(h, "push", signature(hookSecret, body), body); code != http.StatusAccepted {
		t.Fatalf("= %d, want the .git spelling to match: %s", code, got)
	}
	if len(creator.got) != 1 {
		t.Errorf("builds = %d, want 1", len(creator.got))
	}
}

// The same refusal POST /builds gives, for the same reason: this control plane
// is not in the cluster, so there is no ServiceAccount for a Role to be bound
// to. "Cannot" rather than "failed" sends the reader to the chart instead of to
// the retry button.
func TestAVerifiedPushSaysSoWhenTheInstallationCannotBuild(t *testing.T) {
	h := hookEnv(t, nil)
	body := pushBody("refs/heads/main", hookSHA)

	code, got := deliver(h, "push", signature(hookSecret, body), body)
	if code != http.StatusNotImplemented {
		t.Fatalf("= %d, want 501: %s", code, got)
	}
	if !strings.Contains(got, BuildNamespace) {
		t.Errorf("the refusal does not name the namespace a Role is missing for: %s", got)
	}
}

func TestAProviderThisPlatformDoesNotSpeakIsNotFound(t *testing.T) {
	h := hookEnv(t, &collectingCreator{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/gitlab", strings.NewReader("{}"))
	req.Host = testHost
	req.Header.Set(githubEventHeader, "push")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		// 404 and not 400: a forge that gets 400 retries a delivery that can
		// never succeed.
		t.Errorf("an unknown provider = %d, want 404", rec.Code)
	}
}

// A signature is over bytes, so the whole body is read into memory before
// anything is decoded. Without a cap that is a way to spend the server's memory
// from a position that is unauthenticated by design.
func TestADeliveryLargerThanTheCapIsRefused(t *testing.T) {
	h := hookEnv(t, &collectingCreator{})
	body := append([]byte(`{"ref":"refs/heads/main","padding":"`),
		append([]byte(strings.Repeat("x", maxHookBody+1)), []byte(`"}`)...)...)

	code, _ := deliver(h, "push", signature(hookSecret, body), body)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized delivery = %d, want 413", code)
	}
}

// This endpoint is unauthenticated on purpose and must stay out of the table
// TestEveryTenantRouteIsGuarded walks — a route in that table with no session
// check would fail it, and a route outside it that quietly gained one would be
// an endpoint no test walks at all.
//
// Both halves are asserted because only together do they say anything. The
// first is that the hook is not in the table; the second is that it genuinely
// answers a caller with no cookie, which is what makes being outside the table
// correct rather than an oversight.
func TestTheHookIsOutsideTheGuardedTableAndAnswersAnonymously(t *testing.T) {
	for _, rt := range tenantRoutes {
		if strings.Contains(rt.suffix, "hook") {
			t.Errorf("the webhook is in tenantRoutes as %q; it carries no session and would fail the guard", rt.suffix)
		}
	}

	creator := &collectingCreator{}
	h := hookEnv(t, creator)
	body := pushBody("refs/heads/main", hookSHA)
	// No cookie is set anywhere in this file, which is the point.
	if code, got := deliver(h, "push", signature(hookSecret, body), body); code != http.StatusAccepted {
		t.Fatalf("a signed push from a caller with no session = %d, want 202: %s", code, got)
	}
	if len(creator.got) != 1 {
		t.Errorf("builds = %d", len(creator.got))
	}
}

// The response says what was started, because a forge's delivery log is where
// somebody looks when a push did not deploy and the answer has to be readable
// from there.
func TestTheAnswerNamesWhatItStarted(t *testing.T) {
	creator := &collectingCreator{}
	h := hookEnv(t, creator)
	body := pushBody("refs/heads/main", hookSHA)

	_, got := deliver(h, "push", signature(hookSecret, body), body)
	var answer struct {
		Revision string              `json:"revision"`
		Builds   []map[string]string `json:"builds"`
	}
	if err := json.Unmarshal([]byte(got), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, got)
	}
	if answer.Revision != hookSHA {
		t.Errorf("revision = %q", answer.Revision)
	}
	if len(answer.Builds) != 1 || answer.Builds[0]["app"] != appAPI || answer.Builds[0]["env"] != envProd {
		t.Errorf("builds = %+v", answer.Builds)
	}
	if answer.Builds[0]["name"] == "" {
		t.Error("the answer does not name the build that was created")
	}
}

// The padding key's whole purpose is timing, which no assertion in this file
// can check. These two benchmarks are how the claim was measured: a repository
// this install has never heard of must cost what a known one with a wrong
// signature costs, over a body large enough for the difference to matter.
//
// Run with: go test ./server/ -bench 'Verify' -benchtime 2000x
func BenchmarkVerifyUnknownRepository(b *testing.B) {
	benchmarkVerify(b, nil)
}

func BenchmarkVerifyKnownRepositoryWrongSignature(b *testing.B) {
	benchmarkVerify(b, []placement.Trigger{{
		TenantID: tenantAcme, App: appAPI, Env: envProd,
		Provider: providerGitHub, RepoURL: hookRepo, Secret: hookSecret,
	}})
}

func benchmarkVerify(b *testing.B, candidates []placement.Trigger) {
	b.Helper()
	// A megabyte, which is the cap: the difference this measures grows with the
	// body, so measuring a small one would flatter the result.
	body := make([]byte, maxHookBody)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	sig := signature("not-the-secret", body)
	b.ResetTimer()
	for range b.N {
		if got := verifyGitHub(body, sig, candidates); len(got) != 0 {
			b.Fatal("a wrong secret verified")
		}
	}
}

// The joint: a build a push started has to say which environment it is for.
//
// A Build carries the tenant and the app, and nothing else in the cluster
// knows which of an app's environments a repository push belongs to — the
// trigger that matched is the only place the answer exists. Without this label
// the chain completes as far as a digest and stops: buildwatch reads a
// succeeded build, finds no environment, and correctly refuses to guess. Which
// is the shape this whole change is closing, so it gets a case of its own.
func TestAPushBuildsSomethingThatKnowsWhereToDeploy(t *testing.T) {
	creator := &collectingCreator{}
	h := hookEnv(t, creator)
	body := pushBody("refs/heads/main", hookSHA)

	if code, got := deliver(h, "push", signature(hookSecret, body), body); code != http.StatusAccepted {
		t.Fatalf("a correctly signed push = %d: %s", code, got)
	}
	if len(creator.got) != 1 {
		t.Fatalf("builds started = %d, want 1", len(creator.got))
	}
	got := creator.got[0].Labels[buildwatch.EnvLabel]
	if got == "" {
		t.Fatal("the build names no environment, so a digest it produces has nowhere to " +
			"go: buildwatch reads a succeeded build, finds no environment, and stops")
	}
	if got != envProd {
		t.Errorf("environment = %q, want the one the trigger resolved to (%q)", got, envProd)
	}
}
