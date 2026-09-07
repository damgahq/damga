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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/damgahq/damga/placement"
)

// The forge headers this endpoint reads. Named rather than spelled inline
// because two of them are compared against and one is echoed into a log, and a
// header read under three spellings is a header read wrong once.
// providerGitHub is the one forge this endpoint speaks, named once so the path
// value, the store lookup and the signature scheme cannot drift apart.
const providerGitHub = "github"

const (
	githubEventHeader     = "X-GitHub-Event"
	githubSignatureHeader = "X-Hub-Signature-256"
	githubDeliveryHeader  = "X-GitHub-Delivery"
)

// maxHookBody bounds a delivery.
//
// Larger than maxRequestBody, and separately named so the reason survives.
// A push payload carries a commit list, and GitHub caps a push event at 25MB
// while truncating the commit array past 20 entries. This endpoint reads five
// fields and none of them is in that array, so the cap is what a large-but-
// ordinary push needs and not what the forge is willing to send: a monorepo
// merge with a long commit list is routine, and refusing one would be a webhook
// that works until the day somebody merges a branch.
const maxHookBody = 1 << 20

// hookPadding is the key an unknown repository is compared against, so that
// "no such repository" does the same work as "wrong signature".
//
// Minted once at startup and never given out. It is not a secret in the sense
// that anything depends on it staying hidden; it is a key that cannot match, and
// its only job is to make the timing of a miss look like the timing of a
// failure.
var hookPadding = func() []byte {
	b := make([]byte, sha256.Size)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read does not fail on any platform this runs on, and a
		// process that cannot read random bytes has worse problems than this
		// one. Panicking beats silently using a key of zeros, which would be a
		// key an attacker could also compute with.
		panic("server: reading random bytes for the webhook padding key: " + err.Error())
	}
	return b
}()

// deletedRevision is what a forge sends for "after" when a branch is deleted:
// forty zeros rather than an absent field. Building it would clone a commit
// that does not exist.
const deletedRevision = "0000000000000000000000000000000000000000"

// pushEvent is the part of a forge's push payload this platform acts on.
//
// Five fields out of a document with several hundred. Decoded into a named type
// rather than a map so that the fields being read are the fields written down
// here — a map[string]any would make every one of them a lookup that compiles
// whether or not the forge still sends it.
type pushEvent struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`

	Repository struct {
		CloneURL      string `json:"clone_url"`
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
		FullName      string `json:"full_name"`
	} `json:"repository"`
}

// repoURL is the repository a trigger is registered under.
//
// clone_url first because that is the one a person copies out of the forge's
// own "clone" button, and html_url as the fallback for a payload that omits it.
// placement.CanonicalRepo folds the two spellings together, so which one wins
// changes nothing — this order only decides what a log line says.
func (e pushEvent) repoURL() string {
	if e.Repository.CloneURL != "" {
		return e.Repository.CloneURL
	}
	return e.Repository.HTMLURL
}

// hooks turns a git push into a build.
//
// # Why this endpoint is not in tenantRoutes
//
// It cannot be. Every route in that table lives under /api/v1/tenants/{tenant}
// and is admitted by the guard, which reads a session cookie. GitHub does not
// have one and never will: a webhook delivery is an anonymous POST from a
// machine that has never logged in, and there is no tenant in the path because
// the caller does not know which tenant it is posting to — the platform works
// that out from the repository the push names.
//
// So the identity here is the signature and not the session. The secret is
// shared between one repository's webhook and one placement, the payload is
// signed with it, and a delivery that does not verify is refused before
// anything is read out of it. That is a real identity check and it is the whole
// of one; what it is not is the guard, and the difference matters because
// TestEveryTenantRouteIsGuarded walks tenantRoutes and would pass this route by.
// This handler has a different signature from the ones in that table — no guard
// argument — so it cannot be added to it by accident, which is the only
// protection worth having against somebody putting it there to make the
// symmetry look right.
//
// # What it refuses to tell an unauthenticated caller
//
// A caller who has not proved anything must not be able to learn which
// repositories this install builds. So an unknown repository and a bad
// signature answer identically: the same status, the same body, and no branch
// between them that a clock could tell apart before the comparison. Anything
// else turns this into an endpoint for enumerating a competitor's deployments
// one repository name at a time.
func hooks(st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if provider := r.PathValue("provider"); provider != providerGitHub {
			// 404 rather than 400: the path names a provider that does not
			// exist here, and a forge that gets 400 will retry a delivery that
			// can never succeed.
			problem(w, http.StatusNotFound, "bu adda bir webhook sağlayıcısı yok")
			return
		}

		switch event := r.Header.Get(githubEventHeader); event {
		case "push":
		case "ping":
			// The delivery GitHub makes when the webhook is saved. Answered
			// before the signature is checked and without one on purpose: it
			// is what tells the person who just pasted the URL that the URL is
			// right, and making it depend on the secret being right too would
			// report one mistake as the other.
			writeJSON(w, map[string]any{"pong": true})
			return
		case "":
			problem(w, http.StatusBadRequest, "bu uç bir "+githubEventHeader+" başlığı bekliyor")
			return
		default:
			// Accepted and dropped. A repository's webhook is often configured
			// to send everything, and refusing the other forty event types
			// would fill the forge's delivery log with red that means nothing.
			ignore(w, event)
			return
		}

		// The whole body, because a signature is over bytes. Read before
		// anything is decoded, and kept, because decoding from the stream would
		// leave nothing to verify against.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
		if err != nil {
			problem(w, http.StatusRequestEntityTooLarge, "teslimat bu ucun kabul ettiğinden büyük")
			return
		}

		var event pushEvent
		if err := json.Unmarshal(body, &event); err != nil {
			problem(w, http.StatusBadRequest, "teslimat beklenen JSON değil")
			return
		}
		repo := event.repoURL()
		if repo == "" {
			problem(w, http.StatusBadRequest, "teslimat hiçbir depo adı taşımıyor")
			return
		}

		// The repository is read out of a payload nothing has verified yet, and
		// that is safe for exactly one use: choosing which secrets to try. An
		// attacker who names somebody else's repository selects that
		// repository's secrets and then has to produce a MAC under one of them,
		// which is the thing they do not have. What must not happen is any
		// other use of an unverified field, which is why nothing below this
		// point reads `event` again until the signature has verified.
		candidates, err := st.placement.TriggersFor(r.Context(), providerGitHub, repo)
		if err != nil {
			problem(w, http.StatusInternalServerError, "webhook tetikleyicileri okunamadı")
			return
		}
		matched := verifyGitHub(body, r.Header.Get(githubSignatureHeader), candidates)
		if len(matched) == 0 {
			// One answer for three different situations: no such repository,
			// a repository with no webhook registered, and a signature that
			// did not verify. Telling them apart is what would let somebody
			// map the install.
			problem(w, http.StatusUnauthorized, "teslimat doğrulanamadı")
			return
		}

		// From here the payload is authenticated and its fields may be trusted.
		switch {
		case event.Deleted || event.After == deletedRevision:
			// A branch was deleted. There is no commit to build and the
			// forty-zero revision would be cloned as a ref that does not exist.
			ignore(w, "silinmiş bir dalda derlenecek bir şey yok")
			return
		case !isDefaultBranch(event):
			// Only the repository's default branch, and this is a decision
			// rather than a limitation. A build costs the cluster's build quota
			// — one namespace, shared by every tenant — and nothing in a
			// placement yet says which branch an environment tracks, so
			// building every push to every branch would spend that quota on
			// work nobody asked to deploy. The field that would replace this
			// guess is named in the report; until it exists, the honest
			// default is the branch the forge itself calls default.
			ignore(w, "yalnızca varsayılan dala yapılan push'lar derlenir, bu ise "+event.Ref)
			return
		}

		started := make([]startedBuild, 0, len(matched))
		for _, t := range matched {
			// The same function POST /builds calls, with the same rules. A
			// push that produced a Build this endpoint had assembled its own
			// way would be a second definition of what a build is, and the two
			// would drift on the first change to either.
			build, err := buildFor(st.registry, t.TenantID, t.App, createBuildRequest{
				Repo: repo, Revision: event.After,
			})
			if err != nil {
				// The payload verified, so this is the forge sending something
				// this platform cannot build rather than anybody misbehaving —
				// in practice a repository URL whose scheme GitAuth would not
				// accept.
				problem(w, http.StatusBadRequest, err.Error())
				return
			}
			if st.builds == nil {
				// The same refusal createBuild gives, for the same reason and
				// in the same words: this control plane is not in the cluster,
				// so there is no ServiceAccount for a Role to be bound to.
				// Saying "cannot" rather than "failed" is what sends the reader
				// to the chart instead of to the retry button.
				problem(w, http.StatusNotImplemented,
					"bu kurulum derleme başlatamıyor: kontrol düzleminin şurada derleme oluşturma izni yok: "+
						BuildNamespace)
				return
			}
			if err := st.builds.CreateBuild(r.Context(), build); err != nil {
				problem(w, http.StatusBadGateway, "derleme başlatılamadı: "+err.Error())
				return
			}
			started = append(started, startedBuild{
				Tenant: t.TenantID, App: t.App, Env: t.Env,
				Name: build.Name, Namespace: build.Namespace,
				Image: build.Spec.Image, Revision: build.Spec.Revision,
			})
		}

		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{
			"delivery": r.Header.Get(githubDeliveryHeader),
			"revision": event.After,
			"builds":   started,
		})
	})
}

// startedBuild is one line of the answer.
//
// A named type rather than a map, because the same four words — app, namespace,
// revision, image — are keys in three responses in this package, and a key
// spelled by hand in each is a key that disagrees with itself later.
type startedBuild struct {
	Tenant    string `json:"tenant"`
	App       string `json:"app"`
	Env       string `json:"env"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Revision  string `json:"revision"`
}

// ignore answers a delivery this platform read, understood and will not act on.
//
// 202 and not an error: the forge did nothing wrong, and a delivery log full of
// red for pushes that were correctly skipped is a log nobody reads.
func ignore(w http.ResponseWriter, why string) {
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{"ignored": why})
}

// isDefaultBranch says whether the push is to the branch the forge calls
// default.
//
// A payload with no default_branch answers false rather than true. The two
// wrong answers are not symmetrical: guessing true builds every push to every
// branch of a repository whose payload this platform did not understand, and
// guessing false builds nothing and is noticed.
func isDefaultBranch(e pushEvent) bool {
	if e.Repository.DefaultBranch == "" {
		return false
	}
	return e.Ref == "refs/heads/"+e.Repository.DefaultBranch
}

// verifyGitHub returns every trigger whose secret signs this body.
//
// Every one and not the first, because one repository legitimately feeds
// several environments and a person configuring them naturally pastes one
// secret into one webhook. Which environments a delivery is for is decided
// here, by which secrets verify, and nowhere else.
//
// # Constant time
//
// hmac.Equal and never ==. A byte-at-a-time comparison that returns on the
// first difference leaks, through the time it takes, how much of a guess was
// right — which turns forging a MAC from an infeasible search into a
// byte-by-byte one against an endpoint that is unauthenticated by design and
// answers as fast as it can. The loop below runs the same comparison for every
// candidate rather than stopping at the first match, so the number of
// candidates is not learnable either.
func verifyGitHub(body []byte, header string, candidates []placement.Trigger) []placement.Trigger {
	// "sha256=" and then hex, and nothing else accepted.
	//
	// Strict, and honest about why: the comparison below would refuse a
	// malformed header anyway — a digest of the wrong length or the wrong bytes
	// loses to hmac.Equal like any other wrong guess — so this closes no hole.
	// It was written as though it did, and the claim did not survive being
	// tested: taking the strictness out fails nothing. What it buys is that
	// this function refuses a shape it does not understand at the point it
	// notices, instead of passing arbitrary bytes down to a comparison and
	// relying on that to say no.
	digest, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return nil
	}
	want, err := hex.DecodeString(digest)
	if err != nil || len(want) != sha256.Size {
		return nil
	}

	if len(candidates) == 0 {
		// A repository this install has never heard of must cost what a known
		// one costs. Without this the two are told apart by how long the answer
		// took — zero HMACs against a megabyte of body versus one — and the
		// uniform 401 above becomes a formality while the clock keeps
		// answering. The same shape as verifying a password against a dummy
		// hash for an account that does not exist.
		mac := hmac.New(sha256.New, hookPadding)
		_, _ = mac.Write(body)
		if hmac.Equal(mac.Sum(nil), want) {
			// Unreachable: the key is 32 random bytes minted at startup and
			// never handed out. The branch is here so the comparison is live
			// code that a compiler may not drop.
			return nil
		}
		return nil
	}

	var matched []placement.Trigger
	for _, t := range candidates {
		mac := hmac.New(sha256.New, []byte(t.Secret))
		// Hash.Write is documented never to return an error.
		_, _ = mac.Write(body)
		if hmac.Equal(mac.Sum(nil), want) {
			matched = append(matched, t)
		}
	}
	return matched
}
