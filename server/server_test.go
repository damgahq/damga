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

// This file is an external test on purpose. It imports the server package the
// way a second repository does and touches nothing else, so anything it needs
// that is not exported would fail to compile here before it failed to compile
// for a paying customer.
package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/memory"
	"github.com/damgahq/damga/identity"
	identitymem "github.com/damgahq/damga/identity/memory"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
	placementmem "github.com/damgahq/damga/placement/memory"
	"github.com/damgahq/damga/server"
)

// ---------------------------------------------------------------- fakes
//
// These stand in for damga-ee. They are deliberately not clever: what is under
// test is that the seams are reachable and consulted, not that a plausible
// enterprise implementation could be written.

// refusingAuthorizer refuses everything and says so, which is how the test can
// tell that the substituted authorizer was the one that answered rather than
// the free one that would have allowed the read.
type refusingAuthorizer struct{ calls int }

func (a *refusingAuthorizer) Authorize(context.Context, authz.Subject, authz.Action, authz.Target,
) (authz.Decision, error) {
	a.calls++
	return authz.Decision{Allow: false, Reason: "refused by the enterprise policy engine"}, nil
}

// countingStore wraps a real store so the test can assert it was read through
// rather than bypassed.
type countingStore struct {
	evidence.Store
	reads int
}

func (s *countingStore) Current(ctx context.Context, ref evidence.Ref) (evidence.Record, error) {
	s.reads++
	return s.Store.Current(ctx, ref)
}

// ---------------------------------------------------------------- helpers

// start runs the server on a port the kernel picks and returns its base URL.
// Port zero rather than a fixed number, because two tests racing for 8080 is a
// flake that only appears on a busy machine.
func start(t *testing.T, o server.Options) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	o.Config.ListenAddr = "127.0.0.1:0"
	addrCh := make(chan string, 1)
	o.Ready = func(addr string) { addrCh <- addr }

	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(ctx, o) }()

	var addr string
	select {
	case addr = <-addrCh:
	case err := <-runErr:
		cancel()
		t.Fatalf("Run returned before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Run never became ready")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			// A clean shutdown is part of the contract: a control plane that
			// returns an error on SIGTERM makes every rollout look failed.
			if err != nil {
				t.Errorf("Run returned %v on shutdown, want nil", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
	})
	return "http://" + addr
}

func get(t *testing.T, url string, header map[string][]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, string(body)
}

const (
	testApp      = "api"
	testEnv      = "prod"
	testTenant   = "tenant-a"
	testEmail    = "orhan@example.test"
	testPassword = "correct horse battery staple"
)

// identityWith seeds a store with one tenant and one account that has the given
// role in it. An empty role means the account exists but belongs to no tenant,
// which is the case that must NOT resolve to a viewer.
func identityWith(t *testing.T, role identity.Role) identity.Store {
	t.Helper()
	const tenant = testTenant
	ctx := context.Background()
	store := identitymem.New()

	if _, err := store.CreateTenant(ctx, identity.Tenant{
		ID: tenant, Slug: tenant, DisplayName: tenant}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	// Cheap parameters: the suite logs in a dozen times and what is under test
	// is the wiring, not the cost.
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
	if role != "" {
		if err := store.AddMember(ctx, identity.Membership{
			AccountID: "u-1", TenantID: tenant, Role: role,
		}); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}
	return store
}

// login posts credentials and returns the session cookie.
func login(t *testing.T, base string) *http.Cookie {
	t.Helper()
	const password = testPassword
	body := strings.NewReader(`{"email":"` + testEmail + `","password":"` + password + `"}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/api/v1/login", body)
	if err != nil {
		t.Fatalf("building the login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	t.Fatal("login returned no session cookie")
	return nil
}

func cookieHeader(c *http.Cookie) map[string][]string {
	if c == nil {
		return nil
	}
	return map[string][]string{"Cookie": {c.Name + "=" + c.Value}}
}

func seed(t *testing.T, store evidence.Store, ref evidence.Ref) {
	t.Helper()
	ctx := context.Background()
	rec, err := store.Append(ctx, evidence.Record{
		IdempotencyKey: "commit:" + ref.App,
		Ref:            ref,
		Source:         evidence.Source{CommitSHA: "abc123", RepoURL: "https://example.test/r.git"},
	})
	if err != nil {
		t.Fatalf("seeding Append: %v", err)
	}
	for _, to := range []evidence.State{evidence.StateApplied, evidence.StateRunning} {
		from := evidence.StatePending
		if to == evidence.StateRunning {
			from = evidence.StateApplied
		}
		if _, err := store.Transition(ctx, rec.ID, evidence.Transition{
			From: []evidence.State{from}, To: to, At: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seeding Transition to %s: %v", to, err)
		}
	}
}

// ---------------------------------------------------------------- cases

// The claim in the package doc, under test: the zero value is a complete
// installation. If this ever needs a field set, the free tier has acquired a
// gap that the paid build fills, which is the arrangement the plan rejected.
func TestZeroOptionsIsACompleteInstallation(t *testing.T) {
	base := start(t, server.Options{})

	status, body := get(t, base+"/healthz", nil)
	if status != http.StatusOK {
		t.Errorf("GET /healthz = %d %q, want 200", status, body)
	}
}

// The free authorizer is wired by default and actually decides: a viewer may
// read the evidence page, and a subject from another tenant may not.
func TestFreeAuthorizerDecidesByDefault(t *testing.T) {
	store := memory.New(0)
	ref := evidence.Ref{TenantID: testTenant, App: testApp, Env: testEnv}
	seed(t, store, ref)

	base := start(t, server.Options{
		Evidence: store,
		Identity: identityWith(t, identity.RoleViewer),
	})
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base, ref.TenantID, ref.App, ref.Env)

	session := login(t, base)
	status, body := get(t, url, cookieHeader(session))
	if status != http.StatusOK {
		t.Fatalf("a viewer reading its own tenant = %d %q, want 200", status, body)
	}
	var got evidence.Record
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	if got.Source.CommitSHA != "abc123" {
		t.Errorf("returned commit %q, want the seeded one", got.Source.CommitSHA)
	}

	// The same signed-in person, against a tenant they are not a member of.
	other := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base, "tenant-b", ref.App, ref.Env)
	if status, _ := get(t, other, cookieHeader(session)); status != http.StatusForbidden {
		t.Errorf("a member of one tenant reading another = %d, want 403", status)
	}

	if status, _ := get(t, url, nil); status != http.StatusUnauthorized {
		t.Errorf("no session on the request = %d, want 401", status)
	}
}

// The security property, exercised end to end rather than asserted in the store
// alone: a real, valid session for an account that belongs to no tenant must not
// resolve to a viewer — and a viewer may read this page.
func TestASessionWithoutAMembershipIsNotAViewer(t *testing.T) {
	store := memory.New(0)
	ref := evidence.Ref{TenantID: testTenant, App: testApp, Env: testEnv}
	seed(t, store, ref)

	base := start(t, server.Options{
		Evidence: store,
		Identity: identityWith(t, ""), // account exists, no membership
	})
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base, ref.TenantID, ref.App, ref.Env)

	session := login(t, base)
	if status, body := get(t, url, cookieHeader(session)); status != http.StatusForbidden {
		t.Errorf("an account with no membership read the evidence page: %d %q", status, body)
	}
}

// Every failed login says the same thing. Saying "no such account" for one
// address and "wrong password" for another is an enumeration oracle with a user
// interface.
func TestEveryFailedLoginSaysTheSameThing(t *testing.T) {
	base := start(t, server.Options{
		Evidence: memory.New(0),
		Identity: identityWith(t, identity.RoleOwner),
	})

	answers := make([]string, 0, 3)
	for _, body := range []string{
		`{"email":"` + testEmail + `","password":"wrong"}`,
		`{"email":"nobody@example.test","password":"wrong"}`,
		`{"email":"","password":""}`,
	} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			base+"/api/v1/login", strings.NewReader(body))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", body, resp.StatusCode)
		}
		answers = append(answers, string(got))
	}
	for i := 1; i < len(answers); i++ {
		if answers[i] != answers[0] {
			t.Errorf("two failures answered differently:\n%s\n%s", answers[0], answers[i])
		}
	}
}

// Logout revokes the session server-side, not just in the browser. A cookie the
// server still honours after logout is not a logout.
func TestLogoutRevokesTheSession(t *testing.T) {
	store := memory.New(0)
	ref := evidence.Ref{TenantID: testTenant, App: testApp, Env: testEnv}
	seed(t, store, ref)

	base := start(t, server.Options{
		Evidence: store,
		Identity: identityWith(t, identity.RoleOwner),
	})
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base, ref.TenantID, ref.App, ref.Env)

	session := login(t, base)
	if status, _ := get(t, url, cookieHeader(session)); status != http.StatusOK {
		t.Fatalf("setup: the session does not work")
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/api/v1/logout", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.AddCookie(session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/logout: %v", err)
	}
	_ = resp.Body.Close()

	if status, _ := get(t, url, cookieHeader(session)); status != http.StatusUnauthorized {
		t.Errorf("the session still works after logout: %d", status)
	}
}

// The substitution the whole arrangement exists for. A second repository hands
// in its own authorizer and its own store, and both are the ones consulted —
// asserted by their side effects, not by the response alone, because a handler
// that ignored the seam and answered from the free implementation would look
// identical from outside.
func TestEnterpriseCanSubstituteBothSeams(t *testing.T) {
	refuser := &refusingAuthorizer{}
	store := &countingStore{Store: memory.New(0)}
	ref := evidence.Ref{TenantID: testTenant, App: testApp, Env: testEnv}
	seed(t, store.Store, ref)

	base := start(t, server.Options{
		Authorizer: refuser, Evidence: store,
		Identity: identityWith(t, identity.RoleOwner),
	})
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base, ref.TenantID, ref.App, ref.Env)

	status, body := get(t, url, cookieHeader(login(t, base)))
	if status != http.StatusForbidden {
		t.Fatalf("= %d %q, want 403: the substituted authorizer refused", status, body)
	}
	if refuser.calls != 1 {
		t.Errorf("the substituted authorizer was called %d times, want 1", refuser.calls)
	}
	// A refusal must not read the store. Reading first and then deciding is how
	// an authorization bug becomes a data leak in the logs.
	if store.reads != 0 {
		t.Errorf("the store was read %d times on a refused request, want 0", store.reads)
	}
	if !strings.Contains(body, "enterprise policy engine") {
		t.Errorf("the refusal did not carry the authorizer's own reason: %q", body)
	}

	// And when it allows, the substituted store is the one read.
	allower := allowAll{}
	store2 := &countingStore{Store: memory.New(0)}
	seed(t, store2.Store, ref)
	base2 := start(t, server.Options{
		Authorizer: allower, Evidence: store2,
		Identity: identityWith(t, identity.RoleViewer),
	})
	url2 := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base2, ref.TenantID, ref.App, ref.Env)

	if status, body := get(t, url2, cookieHeader(login(t, base2))); status != http.StatusOK {
		t.Fatalf("= %d %q, want 200", status, body)
	}
	if store2.reads != 1 {
		t.Errorf("the substituted store was read %d times, want 1", store2.reads)
	}
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, authz.Subject, authz.Action, authz.Target,
) (authz.Decision, error) {
	return authz.Decision{Allow: true, Reason: "test"}, nil
}

// The hooks: an extra route, a wrapper, and a bundle. All three are what an
// enterprise build needs in order not to fork this package.
func TestHooksAreReachable(t *testing.T) {
	const marker = "X-Enterprise"

	base := start(t, server.Options{
		Authorizer: allowAll{},
		Evidence:   memory.New(0),
		Panel: fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte("<title>enterprise</title>")},
		},
		Routes: func(mux *http.ServeMux) {
			mux.HandleFunc("GET /auth/sso/callback", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("sso"))
			})
		},
		Middleware: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(marker, "1")
				next.ServeHTTP(w, r)
			})
		},
	})

	if status, body := get(t, base+"/auth/sso/callback", nil); status != http.StatusOK || body != "sso" {
		t.Errorf("the Routes hook did not mount: %d %q", status, body)
	}
	if status, body := get(t, base+"/", nil); status != http.StatusOK || !strings.Contains(body, "enterprise") {
		t.Errorf("the substituted panel was not served: %d %q", status, body)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/healthz", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get(marker) == "" {
		t.Error("the Middleware hook did not wrap the core routes")
	}
}

// A middleware that returns nil is a programming error in the enterprise build,
// and it has to fail at startup. Serving nil panics on the first request
// instead, which would be discovered by a customer.
func TestNilMiddlewareIsRefusedAtStartup(t *testing.T) {
	err := server.Run(context.Background(), server.Options{
		Config:     server.Config{ListenAddr: "127.0.0.1:0"},
		Evidence:   memory.New(0),
		Middleware: func(http.Handler) http.Handler { return nil },
	})
	if err == nil {
		t.Fatal("Run accepted a Middleware that returns nil")
	}
	if !strings.Contains(err.Error(), "nil handler") {
		t.Errorf("Run failed with %v, which does not name the cause", err)
	}
}

// An app that has never been deployed is a normal state, not a server error.
// The page has to be able to render it, so it must be distinguishable.
func TestNothingDeployedIsNotAnError(t *testing.T) {
	base := start(t, server.Options{
		Authorizer: allowAll{}, Evidence: memory.New(0),
		Identity: identityWith(t, identity.RoleViewer),
	})
	status, _ := get(t,
		base+"/api/v1/tenants/"+testTenant+"/apps/never/envs/"+testEnv+"/evidence",
		cookieHeader(login(t, base)))
	if status != http.StatusNotFound {
		t.Errorf("= %d, want 404 for an app with no deploys", status)
	}
}

// The interface assertions damga-ee depends on. Written as a compile-time check
// rather than prose, because "implements the interface" is exactly the kind of
// claim that stops being true without anyone noticing.
var (
	_ authz.Authorizer = allowAll{}
	_ authz.Authorizer = (*refusingAuthorizer)(nil)
	_ evidence.Store   = (*countingStore)(nil)
)

// ---------------------------------------------------------------- bootstrap

// What every bootstrap case asks for. The owner's address is deliberately not
// testEmail: these cases create the account rather than finding a seeded one,
// and reusing that name would hide a bootstrap that quietly did nothing.
const (
	bootstrapSlug  = "acme"
	bootstrapEmail = "owner@example.test"
)

// The seam the whole subcommand exists for: an install with nothing in it gets
// an account that can sign in over HTTP with the password that was printed.
// Run against a real file, because the thing being checked is that the two
// processes — the one that bootstraps and the one that serves — agree about
// what is in the database, and an in-memory store cannot disagree.
func TestBootstrapProducesAnOwnerWhoCanSignIn(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "damga.db")
	cfg := server.Config{EvidenceDSN: dsn}

	res, err := server.Bootstrap(t.Context(), cfg, server.BootstrapRequest{
		Email: bootstrapEmail, TenantSlug: bootstrapSlug,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !res.Generated || len(res.Password) < 20 {
		t.Fatalf("generated password is %d characters, generated=%v", len(res.Password), res.Generated)
	}

	base := start(t, server.Options{Config: cfg})

	body := strings.NewReader(`{"email":"` + bootstrapEmail + `","password":"` + res.Password + `"}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/api/v1/login", body)
	if err != nil {
		t.Fatalf("building the login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the bootstrapped owner cannot sign in: login = %d, want 200", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login returned no session cookie")
	}

	// And the owner is an owner of the tenant that was just created — 404
	// rather than 403, because nothing is deployed there yet.
	url := base + "/api/v1/tenants/" + res.TenantID + "/apps/api/envs/prod/evidence"
	if code, _ := get(t, url, cookieHeader(cookie)); code != http.StatusNotFound {
		t.Errorf("evidence in the bootstrapped tenant = %d, want 404", code)
	}
	// Somewhere they are not a member of stays refused.
	other := base + "/api/v1/tenants/t_elsewhere/apps/api/envs/prod/evidence"
	if code, _ := get(t, other, cookieHeader(cookie)); code != http.StatusForbidden {
		t.Errorf("evidence in another tenant = %d, want 403", code)
	}
}

// Running it twice is a mistake, not a failure, and the caller has to be able
// to tell — a deployment script that reruns this needs to continue rather than
// abort, and it cannot parse a message to find that out.
func TestBootstrapRefusesToRunTwice(t *testing.T) {
	cfg := server.Config{EvidenceDSN: filepath.Join(t.TempDir(), "damga.db")}
	req := server.BootstrapRequest{Email: bootstrapEmail, TenantSlug: bootstrapSlug}

	if _, err := server.Bootstrap(t.Context(), cfg, req); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	req.Email, req.TenantSlug = "stranger@example.test", "beta"
	_, err := server.Bootstrap(t.Context(), cfg, req)
	if !errors.Is(err, server.ErrAlreadyBootstrapped) {
		t.Fatalf("a second bootstrap returned %v, want ErrAlreadyBootstrapped", err)
	}
}

// Without a DSN the server keeps identities in memory, so this would report an
// owner into a database that stops existing when the command returns — and the
// operator would find out at the login screen.
func TestBootstrapRefusesAnInstallThatForgets(t *testing.T) {
	_, err := server.Bootstrap(t.Context(), server.Config{}, server.BootstrapRequest{
		Email: bootstrapEmail, TenantSlug: bootstrapSlug,
	})
	if err == nil {
		t.Fatal("bootstrapping without a DSN succeeded; the owner would vanish on exit")
	}
	if !strings.Contains(err.Error(), "-evidence-dsn") {
		t.Errorf("the error does not say which flag is missing: %v", err)
	}
}

// ---------------------------------------------------------------- tier

// capturingAuthorizer records the subject it was handed and then allows, so a
// test can assert what the checkpoint was told rather than what it decided.
type capturingAuthorizer struct{ got authz.Subject }

func (a *capturingAuthorizer) Authorize(
	_ context.Context, s authz.Subject, _ authz.Action, _ authz.Target,
) (authz.Decision, error) {
	a.got = s
	return authz.Decision{Allow: true, Reason: "captured"}, nil
}

// A suspended tenant is refused before any authorizer runs.
//
// Refused here rather than by each authorizer because suspension is a fact
// about the tenant, not a policy: leaving it to the authorizer means the free
// one and a paid one can disagree about whether an unpaid account still works,
// and the disagreement shows up as a customer who was supposed to be cut off
// and was not.
func TestASuspendedTenantIsRefusedBeforeAnyPolicyRuns(t *testing.T) {
	ctx := context.Background()
	idStore := identityWith(t, identity.RoleOwner)

	store := memory.New(0)
	ref := evidence.Ref{TenantID: testTenant, App: testApp, Env: testEnv}
	seed(t, store, ref)
	captured := &capturingAuthorizer{}
	base := start(t, server.Options{
		Evidence: store, Identity: idStore, Authorizer: captured,
	})
	session := login(t, base)
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/evidence",
		base, ref.TenantID, ref.App, ref.Env)

	// While it is running, the owner can read.
	if status, _ := get(t, url, cookieHeader(session)); status != http.StatusOK {
		t.Fatalf("evidence before suspension = %d, want 200", status)
	}

	if err := suspend(ctx, idStore); err != nil {
		t.Fatalf("suspending: %v", err)
	}
	captured.got = authz.Subject{}

	// The existing session is not enough: suspension takes effect on the next
	// request, not on the next login. Anything else means a tenant cut off on
	// Friday keeps deploying all weekend.
	if status, body := get(t, url, cookieHeader(session)); status != http.StatusForbidden {
		t.Errorf("a suspended tenant served the evidence page: %d %q", status, body)
	}
	if captured.got.ID != "" {
		t.Error("the authorizer was consulted about a suspended tenant")
	}
}

func suspend(ctx context.Context, s identity.Store) error {
	return mutateTenant(ctx, s, func(t *identity.Tenant) { t.Suspended = true })
}

func mutateTenant(ctx context.Context, s identity.Store, change func(*identity.Tenant)) error {
	current, err := s.Tenant(ctx, testTenant)
	if err != nil {
		return err
	}
	change(&current)
	_, err = s.UpdateTenant(ctx, current)
	return err
}

// ---------------------------------------------------------------- failures

// failingTenants is an identity store whose tenant lookup is broken and whose
// everything else works, which is what a database going away mid-request looks
// like from here.
type failingTenants struct {
	identity.Store
	fail bool
}

func (s *failingTenants) Tenant(ctx context.Context, id string) (identity.Tenant, error) {
	if s.fail {
		return identity.Tenant{}, errors.New("connection reset by peer")
	}
	return s.Store.Tenant(ctx, id)
}

// A store that is failing must not be reported as an account that belongs
// nowhere.
//
// /me skipped any membership whose tenant it could not read, which is right for
// a row pointing at a deleted tenant and wrong for a store that is down: the
// answer becomes 200 with an empty list, the panel says you are a member of
// nothing, and the reader goes looking at permissions for a fault that is in
// the database.
func TestAFailingStoreIsNotAnEmptyMembershipList(t *testing.T) {
	idStore := &failingTenants{Store: identityWith(t, identity.RoleOwner)}
	base := start(t, server.Options{Evidence: memory.New(0), Identity: idStore})
	session := login(t, base)

	status, body := get(t, base+"/api/v1/me", cookieHeader(session))
	if status != http.StatusOK {
		t.Fatalf("me while healthy = %d, want 200", status)
	}
	if !strings.Contains(body, testTenant) {
		t.Fatalf("a healthy /me does not list the tenant: %s", body)
	}

	idStore.fail = true
	status, body = get(t, base+"/api/v1/me", cookieHeader(session))
	if status != http.StatusInternalServerError {
		t.Errorf("me with a broken store = %d %q, want 500", status, body)
	}
	if strings.Contains(body, `"memberships":[]`) {
		t.Error("a broken store answered with an empty membership list")
	}
}

// A cursor the caller made up is the caller's mistake.
//
// It reached the store, failed to parse, and came back as a 500 — which tells
// the client the server is broken and it should retry, and neither is true.
func TestAMadeUpPageCursorIsTheCallersMistake(t *testing.T) {
	store := memory.New(0)
	ref := evidence.Ref{TenantID: testTenant, App: testApp, Env: testEnv}
	seed(t, store, ref)
	base := start(t, server.Options{
		Evidence: store, Identity: identityWith(t, identity.RoleOwner),
	})
	session := login(t, base)
	prefix := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/history",
		base, ref.TenantID, ref.App, ref.Env)

	if status, body := get(t, prefix+"?after=not-a-cursor", cookieHeader(session)); status != http.StatusBadRequest {
		t.Errorf("a made-up cursor = %d %q, want 400", status, body)
	}
	// And a limit that is not a number, which is the same class of mistake.
	if status, _ := get(t, prefix+"?limit=lots", cookieHeader(session)); status != http.StatusBadRequest {
		t.Errorf("a non-numeric limit = %d, want 400", status)
	}
	// The page itself still works, so the two checks above cannot be passing
	// by refusing everything.
	if status, _ := get(t, prefix, cookieHeader(session)); status != http.StatusOK {
		t.Errorf("history = %d, want 200", status)
	}
}

// ---------------------------------------------------------------- deploy

// testNamespace is where the deploy cases say their manifests run.
const (
	testNamespace = "acme-prod"
	testPath      = "apps/api/prod"
	testBranch    = "main"
)

// localAuth is the GitAuth an install would supply for a repository that needs
// no credentials. It also proves Options.GitAuth is reachable from outside the
// package, which is the whole point of the seam being exported.
type localAuth struct{}

func (localAuth) For(string) (transport.AuthMethod, error) { return nil, nil }

// bareRepo makes a repository with one commit on main and returns its path.
func bareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "state.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init: %v", err)
	}
	// A bare repository has no worktree and an empty one cannot be cloned, so
	// the first commit is made beside it and pushed.
	work := filepath.Join(dir, "seed")
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
	who := &object.Signature{Name: "seed", Email: "seed@example.test", When: time.Now()}
	if _, err := tree.Commit("seed", &git.CommitOptions{Author: who, Committer: who}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	head, err := seed.Head()
	if err != nil {
		t.Fatalf("seed head: %v", err)
	}
	if err := seed.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), head.Hash()),
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

// committedFile reads one path out of the remote's main branch.
func committedFile(t *testing.T, repoPath, name string) string {
	t.Helper()
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	f, err := commit.File(name)
	if err != nil {
		t.Fatalf("file %s: %v", name, err)
	}
	body, err := f.Contents()
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	return body
}

// The whole write path, end to end: an owner asks for an image, a commit
// appears on the branch carrying a rendered Workload, and an evidence record
// exists that names it.
func TestADeployCommitsAWorkloadAndOpensARecord(t *testing.T) {
	repo := bareRepo(t)
	store := memory.New(0)
	places := placementmem.New()
	if _, err := places.Put(t.Context(), placement.Placement{
		TenantID: testTenant, App: testApp, Env: testEnv,
		RepoURL: repo, Branch: testBranch, Path: testPath, Namespace: testNamespace,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	base := start(t, server.Options{
		Evidence: store, Placement: places, GitAuth: localAuth{},
		Identity: identityWith(t, identity.RoleOwner),
	})
	session := login(t, base)
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/deploys",
		base, testTenant, testApp, testEnv)

	status, body := post(t, url, `{"image":"ghcr.io/acme/api:1.4.2","replicas":3}`, session)
	if status != http.StatusAccepted {
		t.Fatalf("deploy = %d %q, want 202", status, body)
	}
	// Accepted and not OK, and syncing and not running: at this point the only
	// thing that is true is that a commit was pushed. Whether it reached the
	// cluster is the observer's answer, and it has not given one yet.
	if !strings.Contains(body, `"state":"syncing"`) {
		t.Errorf("the deploy claimed something other than syncing: %s", body)
	}
	if strings.Contains(body, `"state":"running"`) {
		t.Errorf("the deploy claimed the app is running: %s", body)
	}
	// The commit reaches the reader where a reader looks for it, even though
	// the record was opened before the commit existed and Source could never
	// hold it.
	if !strings.Contains(body, `"commitSha":"`) || strings.Contains(body, `"commitSha":""`) {
		t.Errorf("the response carries no commit: %s", body)
	}

	committed := committedFile(t, repo, testPath+"/"+manifest.File)
	for _, want := range []string{
		"kind: Workload",
		"image: ghcr.io/acme/api:1.4.2",
		"namespace: " + testNamespace,
		"replicas: 3",
		"damga.co/rollout:",
	} {
		if !strings.Contains(committed, want) {
			t.Errorf("the committed manifest has no %q:\n%s", want, committed)
		}
	}

	// The record and the commit agree about which commit it was.
	rec, err := store.Current(t.Context(), evidence.Ref{
		TenantID: testTenant, App: testApp, Env: testEnv,
	})
	if err != nil {
		// Current only answers for applied or running; the record is pending,
		// so read it through history instead.
		page, hErr := store.History(t.Context(), evidence.Query{Ref: evidence.Ref{
			TenantID: testTenant, App: testApp, Env: testEnv,
		}})
		if hErr != nil || len(page.Records) != 1 {
			t.Fatalf("no record was opened: %v / %v", err, hErr)
		}
		rec = page.Records[0]
	}
	// In the store it is on the transition, not on Source — Source is the
	// immutable half and the record predates the commit.
	var sha string
	for _, e := range rec.Transitions {
		if e.Observation.Revision != "" {
			sha = e.Observation.Revision
		}
	}
	if sha == "" {
		t.Error("the record does not name the commit it opened")
	}
}

// A second deploy changes only what it asked to change. Everything else is
// read back out of the committed manifest, which is the state — so a deploy
// that bumps the image cannot silently take the replica count with it.
func TestADeployKeepsWhatItDidNotMention(t *testing.T) {
	repo := bareRepo(t)
	places := placementmem.New()
	if _, err := places.Put(t.Context(), placement.Placement{
		TenantID: testTenant, App: testApp, Env: testEnv,
		RepoURL: repo, Branch: testBranch, Path: testPath, Namespace: testNamespace,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	base := start(t, server.Options{
		Evidence: memory.New(0), Placement: places, GitAuth: localAuth{},
		Identity: identityWith(t, identity.RoleOwner),
	})
	session := login(t, base)
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/deploys",
		base, testTenant, testApp, testEnv)

	if status, body := post(t, url,
		`{"image":"ghcr.io/acme/api:1.0.0","replicas":4,"domain":"api.acme.example","port":9000}`,
		session); status != http.StatusAccepted {
		t.Fatalf("first deploy = %d %q", status, body)
	}
	if status, body := post(t, url, `{"image":"ghcr.io/acme/api:2.0.0"}`, session); status != http.StatusAccepted {
		t.Fatalf("second deploy = %d %q", status, body)
	}

	committed := committedFile(t, repo, testPath+"/"+manifest.File)
	for _, want := range []string{
		"image: ghcr.io/acme/api:2.0.0",
		"replicas: 4",
		"domain: api.acme.example",
		"port: 9000",
	} {
		if !strings.Contains(committed, want) {
			t.Errorf("the second deploy lost %q:\n%s", want, committed)
		}
	}
}

// An app nobody has told the control plane where to write is a 409 that says
// so, not a 500 and not a silent success.
func TestADeployWithNoPlacementSaysWhatIsMissing(t *testing.T) {
	base := start(t, server.Options{
		Evidence: memory.New(0), Placement: placementmem.New(), GitAuth: localAuth{},
		Identity: identityWith(t, identity.RoleOwner),
	})
	session := login(t, base)
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/deploys",
		base, testTenant, testApp, testEnv)

	status, body := post(t, url, `{"image":"ghcr.io/acme/api:1.0.0"}`, session)
	if status != http.StatusConflict {
		t.Errorf("deploy without a placement = %d %q, want 409", status, body)
	}
	if !strings.Contains(body, "bir depo tanımlı değil") {
		t.Errorf("the refusal does not say what is missing: %s", body)
	}
}

// A viewer may read the evidence page and may not deploy. The free authorizer
// already draws that line; this is the check that the deploy endpoint asks it.
func TestAViewerCannotDeploy(t *testing.T) {
	repo := bareRepo(t)
	places := placementmem.New()
	if _, err := places.Put(t.Context(), placement.Placement{
		TenantID: testTenant, App: testApp, Env: testEnv,
		RepoURL: repo, Branch: testBranch, Path: testPath, Namespace: testNamespace,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	base := start(t, server.Options{
		Evidence: memory.New(0), Placement: places, GitAuth: localAuth{},
		Identity: identityWith(t, identity.RoleViewer),
	})
	session := login(t, base)
	url := fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/deploys",
		base, testTenant, testApp, testEnv)

	if status, body := post(t, url, `{"image":"ghcr.io/acme/api:1.0.0"}`, session); status != http.StatusForbidden {
		t.Errorf("a viewer deployed: %d %q", status, body)
	}
	// And nothing reached the repository.
	repoObj, err := git.PlainOpen(repo)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ref, err := repoObj.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	commit, err := repoObj.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if commit.Message != "seed" {
		t.Errorf("a refused deploy still committed: %q", commit.Message)
	}
}

// post sends JSON with the session cookie and returns the status and body.
func post(t *testing.T, url, body string, cookie *http.Cookie) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return resp.StatusCode, string(out)
}
