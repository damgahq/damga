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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/placement"
)

const (
	triggerSuffix = "/apps/{app}/envs/{env}/trigger"
	// Named once: these two words appear in several refusals, and three
	// spellings of one literal in a package is what goconst is for.
	wordRepository = "repository"
)

func (l *lifecycle) trigger(account, body string) (int, string) {
	l.t.Helper()
	mux := http.NewServeMux()
	mux.Handle(http.MethodPut+" "+tenantScope+triggerSuffix, setTrigger(l.guard, l.stores))
	target := strings.NewReplacer("{tenant}", tenantHome, "{app}", appAPI, "{env}", envProd).
		Replace(tenantScope + triggerSuffix)
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(body))
	req.Host = testHost
	if account != "" {
		req.AddCookie(l.cookies[account])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// Every refusal names itself, and two of them are the endpoint refusing to
// create something that would silently never work.
func TestConnectingARepositoryIsRefusedByName(t *testing.T) {
	l := newLifecycle(t)
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"no provider", `{"repoUrl":"https://example.test/x.git","secret":"0123456789abcdef"}`,
			[]string{"provider"}},
		{"a forge nothing routes", `{"provider":"gitlab","repoUrl":"https://x.test/x.git","secret":"0123456789abcdef"}`,
			[]string{"github"}},
		{"no " + wordRepository, `{"provider":"github","secret":"0123456789abcdef"}`,
			[]string{wordRepository}},
		{"no secret", `{"provider":"github","repoUrl":"https://example.test/x.git"}`,
			[]string{"secret", "any payload"}},
		{"a secret too short to be one", `{"provider":"github","repoUrl":"https://example.test/x.git","secret":"short"}`,
			[]string{"16", "stranger"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := l.trigger(accOwner, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("accepted %s: %d %s", tc.name, code, body)
			}
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("the refusal does not say %q: %s", want, body)
				}
			}
		})
	}
}

// The secret is write-only, and the response shape is the guarantee.
func TestTheWebhookSecretIsNeverReturned(t *testing.T) {
	const secret = "hunter2-hunter2-hunter2"
	l := newLifecycle(t)

	code, body := l.trigger(accOwner,
		`{"provider":"github","repoUrl":"https://example.test/x.git","secret":"`+secret+`"}`)
	if code != http.StatusOK {
		t.Fatalf("connecting answered %d: %s", code, body)
	}
	if strings.Contains(body, secret) {
		t.Errorf("the response echoed the secret. It is stored in plaintext because an HMAC "+
			"needs the key, which is exactly why nothing may hand it back: %s", body)
	}
	if !strings.Contains(body, "/hooks/github") {
		t.Errorf("the response does not say where to deliver the webhook: %s", body)
	}

	// And it really was stored, or the endpoint is a no-op that answers 200.
	got, err := l.places.TriggersFor(context.Background(), providerGitHub, "https://example.test/x.git")
	if err != nil || len(got) != 1 {
		t.Fatalf("the trigger the webhook path reads is not there: %v %+v", err, got)
	}
	if got[0].Secret != secret || got[0].Env != envProd {
		t.Errorf("stored the wrong trigger: env=%q secretMatches=%v", got[0].Env, got[0].Secret == secret)
	}
}

// Connecting a repository is owner-only, because it hands out a key.
func TestConnectingARepositoryIsOwnerOnly(t *testing.T) {
	l := newLifecycle(t)
	body := `{"provider":"github","repoUrl":"https://example.test/x.git","secret":"0123456789abcdef"}`
	if code, resp := l.trigger(accOwner, body); code != http.StatusOK {
		t.Fatalf("an owner was refused: %d %s", code, resp)
	}
	for _, who := range []string{accMember, accViewer} {
		if code, resp := l.trigger(who, body); code != http.StatusForbidden {
			t.Errorf("%s connected a repository (%d). The secret lets whoever holds it make "+
				"this platform build and ship code with no session at all: %s", who, code, resp)
		}
	}
}

// The idempotence the whole chain rests on.
//
// buildwatch cannot mark a Build as handled — no patch on builds — so after a
// restart it offers the same succeeded build again. gitwrite cannot refuse the
// repeat either, because Render stamps a fresh rollout id and the bytes always
// differ. If the deployer did not compare against what is committed, every
// restart would be another commit of the same digest.
func TestDeployingTheSameDigestTwiceCommitsOnce(t *testing.T) {
	l := newLifecycle(t)
	d := buildDeployer{
		places:  l.places,
		writer:  &gitwrite.Writer{Evidence: l.records},
		gitAuth: passthroughAuth{},
	}
	const image = "reg.test/t_home/api@sha256:abcdef"

	wrote, err := d.Deploy(context.Background(), tenantHome, appAPI, envProd, image)
	if err != nil || !wrote {
		t.Fatalf("the first deploy did not write: wrote=%v err=%v", wrote, err)
	}

	wrote, err = d.Deploy(context.Background(), tenantHome, appAPI, envProd, image)
	if err != nil {
		t.Fatalf("the second deploy failed: %v", err)
	}
	if wrote {
		t.Error("the same digest was committed twice. buildwatch has nowhere to record that " +
			"a build is done, so this is the only thing standing between a restart and a " +
			"commit per resync")
	}

	// A different digest still deploys, or the guard has become a wall.
	wrote, err = d.Deploy(context.Background(), tenantHome, appAPI, envProd,
		"reg.test/t_home/api@sha256:999999")
	if err != nil || !wrote {
		t.Errorf("a new digest was not deployed: wrote=%v err=%v", wrote, err)
	}
}

// A build for an app whose placement is gone is an answer, not a failure.
func TestDeployingIntoAnAppThatIsNoLongerRegisteredIsNotAnError(t *testing.T) {
	l := newLifecycle(t)
	d := buildDeployer{
		places: l.places, writer: &gitwrite.Writer{Evidence: l.records}, gitAuth: passthroughAuth{},
	}
	wrote, err := d.Deploy(context.Background(), tenantHome, "gone", envProd, "reg.test/x@sha256:a")
	if err != nil {
		t.Errorf("a missing placement was reported as a failure, so the build would be "+
			"retried for ever: %v", err)
	}
	if wrote {
		t.Error("it committed into an app with no placement")
	}
	_ = evidence.Ref{}
	_ = placement.Trigger{}
}
