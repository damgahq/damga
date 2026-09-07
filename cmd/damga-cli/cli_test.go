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

package main

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/identity"
	identitymem "github.com/damgahq/damga/identity/memory"
	placementmem "github.com/damgahq/damga/placement/memory"
	"github.com/damgahq/damga/server"
)

const (
	testTenant   = "tenant-a"
	testApp      = "api"
	testEnv      = "prod"
	testEmail    = "orhan@example.test"
	testPassword = "correct horse battery staple"
)

// startControlPlane runs the real server and returns its base URL.
//
// The real one, not a stand-in. The whole claim this CLI makes is that it calls
// the API the panel calls, and a fake server would answer whatever this test
// wrote into it — including endpoints the control plane does not have, which is
// precisely the failure the route table exists to prevent.
func startControlPlane(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	opts := server.Options{
		Evidence:  memory.New(0),
		Identity:  seededIdentity(t),
		Placement: placementmem.New(),
	}
	// Port zero, so two tests do not race for a fixed number on a busy machine.
	opts.Config.ListenAddr = "127.0.0.1:0"

	addrCh := make(chan string, 1)
	opts.Ready = func(addr string) { addrCh <- addr }
	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(ctx, opts) }()

	var addr string
	select {
	case addr = <-addrCh:
	case err := <-runErr:
		cancel()
		t.Fatalf("the control plane returned before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the control plane never became ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(20 * time.Second):
			t.Error("the control plane did not stop")
		}
	})
	return "http://" + addr
}

// seededIdentity is one tenant and one owner.
func seededIdentity(t *testing.T) identity.Store {
	t.Helper()
	ctx := context.Background()
	store := identitymem.New()

	if _, err := store.CreateTenant(ctx, identity.Tenant{
		ID: testTenant, Slug: testTenant, DisplayName: "Tenant A",
	}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	// Cheap parameters. What is under test is the wiring, not the cost of a
	// hash, and the suite logs in on nearly every case.
	hash, err := auth.NewHasher(auth.Params{
		Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}, 2).Hash(testPassword)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if _, err := store.CreateAccount(ctx, identity.Account{
		ID: "u-1", Kind: "user", Email: testEmail,
		AuditEmail: "u-1@users.damga.local", DisplayName: "Orhan Yavuz",
	}, identity.Credential{Hash: hash}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := store.AddMember(ctx, identity.Membership{
		AccountID: "u-1", TenantID: testTenant, Role: identity.RoleOwner,
	}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	return store
}

// cli drives the whole program the way a terminal does.
type cli struct {
	t           *testing.T
	sessionFile string
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	// Cleared rather than left alone: a developer with DAMGA_SERVER exported
	// would otherwise run this suite against their own control plane.
	t.Setenv("DAMGA_SERVER", "")
	t.Setenv("DAMGA_TENANT", "")
	t.Setenv("DAMGA_SESSION_FILE", "")
	// Two levels down, so every test that saves a session also exercises
	// saveSession creating the directory — which is where the mode that
	// protects the token is set.
	return &cli{t: t, sessionFile: filepath.Join(t.TempDir(), "config", "damga", "session.json")}
}

func (c *cli) run(stdin string, args ...string) (string, string, error) {
	c.t.Helper()
	var out, errOut bytes.Buffer
	root := newRoot(&out, &errOut, strings.NewReader(stdin))
	root.SetArgs(append([]string{"--session-file", c.sessionFile}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// mustRun fails the test if the command did not succeed.
func (c *cli) mustRun(stdin string, args ...string) string {
	c.t.Helper()
	out, errOut, err := c.run(stdin, args...)
	if err != nil {
		c.t.Fatalf("damga-cli %s: %v\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, out, errOut)
	}
	return out
}

func (c *cli) login(base string) {
	c.t.Helper()
	c.mustRun(testPassword, "login", "--server", base, "--email", testEmail, "--password-stdin")
}

// TestEveryRouteTheCLICallsExistsOnTheServer walks the table in client.go and
// asks a real control plane for every row.
//
// This is what makes "the CLI calls the same API the panel calls" a fact rather
// than an intention. A route this CLI believes in and the control plane does not
// have reaches the mux's own 404 — plain text, not the API's JSON problem
// document — and that difference is what this asserts on, because several of
// these endpoints answer a legitimate 404 of their own: asking for the current
// deploy of an app that has never been deployed is a 404 and is not a missing
// route.
//
// Reverting the fix this test is bound to — adding an endpoint to the CLI that
// the API does not serve — fails here, naming the method and the path.
func TestEveryRouteTheCLICallsExistsOnTheServer(t *testing.T) {
	base := startControlPlane(t)
	c := newCLI(t)
	c.login(base)

	sess, err := loadSession(c.sessionFile)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	tgt := target{tenant: testTenant, app: testApp, env: testEnv}

	ask := func(rt route) {
		path, err := tgt.fill(rt.pattern)
		if err != nil {
			t.Fatalf("%s: %v", rt, err)
		}
		req, err := http.NewRequestWithContext(context.Background(), rt.method, base+path, nil)
		if err != nil {
			t.Fatalf("%s: %v", rt, err)
		}
		req.AddCookie(&http.Cookie{Name: sess.CookieName, Value: sess.Cookie})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", rt, err)
		}
		contentType := resp.Header.Get("Content-Type")
		_ = resp.Body.Close()

		// 401 would mean the session did not travel and 403 would mean the
		// guard refused an owner. Both are this test being wrong rather than
		// the route being absent, so they are called out separately — an
		// assertion that cannot tell those apart passes for the wrong reason
		// the day somebody breaks the guard.
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			t.Errorf("%s %s answered %d: the test's own session did not work",
				rt.method, path, resp.StatusCode)
			return
		}
		if (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed) &&
			!strings.HasPrefix(contentType, "application/json") {
			t.Errorf("the CLI calls %s %s and the control plane has no such endpoint "+
				"(%d, %q): the CLI may only use the API the panel uses",
				rt.method, path, resp.StatusCode, contentType)
		}
	}

	for _, rt := range routes {
		// Logout last, because it is the one route whose success destroys the
		// credential every other row needs. Asked for in the loop it revoked
		// the session on its second iteration and turned the other twelve into
		// 401s — which the branch above reported as what it was.
		if rt != routeLogout {
			ask(rt)
		}
	}
	ask(routeLogout)
}

// TestLoginThenTheOrdinaryCommands is the path a person actually walks.
func TestLoginThenTheOrdinaryCommands(t *testing.T) {
	base := startControlPlane(t)
	c := newCLI(t)

	c.login(base)

	if got := c.mustRun("", "whoami"); !strings.Contains(got, testTenant) {
		t.Errorf("whoami did not name the tenant this account belongs to:\n%s", got)
	}

	// Nothing registered yet, and the server says so through an empty list
	// rather than a failure.
	if got := c.mustRun("", "apps"); !strings.Contains(got, "No apps") {
		t.Errorf("apps on an empty tenant printed:\n%s", got)
	}

	c.mustRun("", "apps", "create",
		"--app", testApp, "--env", testEnv,
		"--repo", "https://example.test/acme/state", "--branch", "main",
		"--path", "apps/api/prod", "--namespace", "acme-prod")

	listed := c.mustRun("", "apps")
	for _, want := range []string{testApp, testEnv, "hiç deploy edilmedi", "acme-prod"} {
		if !strings.Contains(listed, want) {
			t.Errorf("apps did not mention %q:\n%s", want, listed)
		}
	}

	// An app with no deploys: history is an empty page and status is the
	// ordinary "nothing yet", and neither is a failure.
	if got := c.mustRun("", "history", testApp, testEnv); !strings.Contains(got, "Nothing has been deployed") {
		t.Errorf("history on a fresh app printed:\n%s", got)
	}
	if got := c.mustRun("", "status", testApp, testEnv); !strings.Contains(got, "Nothing has been deployed") {
		t.Errorf("status on a fresh app printed:\n%s", got)
	}

	if got := c.mustRun("", "verify", testApp, testEnv); !strings.Contains(got, "verified") {
		t.Errorf("verify on a fresh app printed:\n%s", got)
	}
	if got := c.mustRun("", "retention", testApp, testEnv); !strings.Contains(got, "kept for ever") {
		t.Errorf("retention printed:\n%s", got)
	}

	removed := c.mustRun("", "apps", "delete", testApp)
	if !strings.Contains(removed, "Still running, and still committed") {
		t.Errorf("apps delete did not say what it left behind:\n%s", removed)
	}
	// The record and not the app: the deleted registration leaves no trace in
	// this install because nothing was ever deployed, so the list is empty
	// again rather than showing "record removed".
	if got := c.mustRun("", "apps"); !strings.Contains(got, "No apps") {
		t.Errorf("apps after delete printed:\n%s", got)
	}
}

// TestASessionIsRefusedByAHostItWasNotIssuedFor pins the message.
//
// The control plane answers a host mismatch with the same "not signed in" it
// gives an expired session, deliberately, so that nothing can be learned from
// the difference. That is right for a stranger and useless for the person who
// typed the wrong hostname, so the CLI — which knows both hosts — says which.
// Reverting that check turns this into "not signed in" and this test fails.
func TestASessionIsRefusedByAHostItWasNotIssuedFor(t *testing.T) {
	base := startControlPlane(t)
	c := newCLI(t)
	c.login(base)

	// The same machine and the same port; a different name. The server strips
	// the port before comparing, so this is the smallest difference that
	// actually breaks a session.
	elsewhere := strings.Replace(base, "127.0.0.1", "localhost", 1)
	_, _, err := c.run("", "--server", elsewhere, "whoami")
	if err == nil {
		t.Fatal("a session issued for 127.0.0.1 was accepted for localhost")
	}
	if !strings.Contains(err.Error(), "bound to the host that issued it") {
		t.Errorf("the refusal does not explain the host binding: %v", err)
	}
	if got := exitFor(err); got != exitNotSignedIn {
		t.Errorf("exit code %d, want %d", got, exitNotSignedIn)
	}
}

// TestAWrongPasswordIsRefusedInTheServersOwnWords.
//
// The message is deliberately the same for a wrong password and an address with
// no account, and reproducing it here is what would catch a client that decided
// to be more helpful.
func TestAWrongPasswordIsRefusedInTheServersOwnWords(t *testing.T) {
	base := startControlPlane(t)
	c := newCLI(t)

	_, _, err := c.run("not the password",
		"login", "--server", base, "--email", testEmail, "--password-stdin")
	if err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if !strings.Contains(err.Error(), "bir hesapla eşleşmiyor") {
		t.Errorf("the refusal was reworded: %v", err)
	}
	if got := exitFor(err); got != exitNotSignedIn {
		t.Errorf("exit code %d, want %d", got, exitNotSignedIn)
	}
	if _, err := loadSession(c.sessionFile); err != errNoSessionFile {
		t.Errorf("a failed login left a session file behind: %v", err)
	}
}

// TestCommandsBeforeLoginSayToLogIn rather than reporting a missing file.
func TestCommandsBeforeLoginSayToLogIn(t *testing.T) {
	c := newCLI(t)
	_, _, err := c.run("", "apps")
	if err == nil {
		t.Fatal("apps worked with no session")
	}
	if !strings.Contains(err.Error(), "login") {
		t.Errorf("the message does not say what to do: %v", err)
	}
	if got := exitFor(err); got != exitNotSignedIn {
		t.Errorf("exit code %d, want %d", got, exitNotSignedIn)
	}
}
