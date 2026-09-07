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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/identity"
)

// guard is the one place a request becomes a subject and an authorization.
//
// It exists because the alternative is every handler repeating the same four
// steps, and four copies of a security check are three chances to write it
// slightly differently. The one that would differ is whichever endpoint gets
// added in a hurry — and the free authorizer treats an unrecognised group as a
// viewer, so getting it wrong does not fail closed. It reads another tenant's
// deploy history.
type guard struct {
	authorizer authz.Authorizer
	identity   identity.Store
	sessions   *auth.Sessions
}

// admit answers who is asking, whether this request may do this, and to what.
// It writes the refusal itself and reports false, so a handler that forgets to
// stop has already sent the right status and cannot also send a body.
//
// The subject came back when the first endpoint that writes arrived: a deploy
// is recorded against the person who asked for it, and that person has to be
// the one the session named rather than one a request body claims.
func (g guard) admit(
	w http.ResponseWriter, r *http.Request, action authz.Action,
) (authz.Subject, evidence.Ref, bool) {
	ref := evidence.Ref{
		TenantID: r.PathValue("tenant"),
		App:      r.PathValue("app"),
		Env:      r.PathValue("env"),
	}

	live, err := g.sessions.Resolve(r.Context(), r)
	if err != nil {
		problem(w, http.StatusUnauthorized, "giriş yapılmadı")
		return authz.Subject{}, ref, false
	}

	sub, err := subjectFrom(r.Context(), g.identity, live, ref.TenantID)
	if err != nil {
		// Deliberately the same answer as a refusal. "You are signed in but
		// not a member here" confirms the tenant exists, which is the one
		// thing a stranger probing tenant names wants to learn.
		problem(w, http.StatusForbidden, "bu kiracıya erişim yok")
		return authz.Subject{}, ref, false
	}

	decision, err := g.authorizer.Authorize(r.Context(), sub, action, authz.Target{
		Tenant: ref.TenantID, App: ref.App, Env: ref.Env,
	})
	if err != nil {
		problem(w, http.StatusInternalServerError, "yetkilendirme başarısız oldu")
		return authz.Subject{}, ref, false
	}
	if !decision.Allow {
		// The reason is returned, because a refusal nobody can explain is a
		// support ticket. It is the authorizer's own words: the free one says
		// which role was read, an enterprise one can say which policy matched.
		problem(w, http.StatusForbidden, decision.Reason)
		return authz.Subject{}, ref, false
	}
	return sub, ref, true
}

// me is who the session belongs to and where they are a member.
//
// The panel needs it to render anything at all, and it is the one endpoint
// that takes no tenant: the membership list IS the answer, so there is nothing
// to authorize against. What it must not do is accept a tenant and describe
// it, which would make it a directory of every tenant on the install.
func (o Options) me(idStore identity.Store, sess *auth.Sessions) http.Handler {
	type membership struct {
		TenantID   string `json:"tenantId"`
		TenantSlug string `json:"tenantSlug"`
		TenantName string `json:"tenantName"`
		Role       string `json:"role"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		live, err := sess.Resolve(r.Context(), r)
		if err != nil {
			problem(w, http.StatusUnauthorized, "giriş yapılmadı")
			return
		}
		account, err := idStore.Account(r.Context(), live.AccountID)
		if err != nil || account.Disabled {
			// A session whose account was deleted or disabled since it was
			// issued. Not signed in, as far as anything downstream is
			// concerned.
			problem(w, http.StatusUnauthorized, "giriş yapılmadı")
			return
		}
		rows, err := idStore.Memberships(r.Context(), account.ID)
		if err != nil {
			problem(w, http.StatusInternalServerError, "kimlik deposu kullanılamıyor")
			return
		}

		out := make([]membership, 0, len(rows))
		for _, m := range rows {
			t, err := idStore.Tenant(r.Context(), m.TenantID)
			switch {
			case errors.Is(err, identity.ErrNotFound):
				// A membership pointing at a tenant that is gone. Skipped
				// rather than failed: one broken row must not make the panel
				// unusable for every other tenant this person belongs to.
				continue
			case err != nil:
				// Anything else is the store failing, and skipping it would
				// answer 200 with an empty list — "you are a member of
				// nothing", which reads as a permissions problem and sends
				// the reader somewhere the fault is not.
				problem(w, http.StatusInternalServerError, "kimlik deposu kullanılamıyor")
				return
			}
			out = append(out, membership{
				TenantID: t.ID, TenantSlug: t.Slug, TenantName: t.DisplayName,
				Role: string(m.Role),
			})
		}
		writeJSON(w, map[string]any{
			"account": map[string]string{
				"id": account.ID, "email": account.Email, "displayName": account.DisplayName,
			},
			"memberships": out,
		})
	})
}

// maxHistoryPage bounds one response. Without a cap, `?limit=1000000` is a way
// to ask for the whole table in one query, which is a slow read holding a
// connection rather than an error anybody notices.
const (
	maxHistoryPage     = 200
	defaultHistoryPage = 50
)

// history is the deploy log for one app in one environment, newest first.
func history(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionEvidenceView)
		if !ok {
			return
		}

		limit := defaultHistoryPage
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				problem(w, http.StatusBadRequest, "limit pozitif bir tam sayı olmalı")
				return
			}
			limit = min(n, maxHistoryPage)
		}

		page, err := st.evidence.History(r.Context(), evidence.Query{
			Ref: ref, Limit: limit,
			After: evidence.Cursor(r.URL.Query().Get("after")),
			Order: evidence.OrderNewest,
		})
		switch {
		case errors.Is(err, evidence.ErrInvalid):
			// A cursor the store cannot resolve. The caller sent it, so this
			// is a 400 — answering 500 tells them the server is broken and
			// they should retry, and it is not and they should not.
			problem(w, http.StatusBadRequest, "bu sayfa imleci bu kaydın verdiği bir imleç değil")
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "kanıt deposu okunamadı")
			return
		}
		// An app with no deploys is an empty page, not a 404. Current answers
		// 404 because "what is running" has no answer; "what has happened" has
		// one, and it is nothing.
		records := make([]wireRecord, 0, len(page.Records))
		for _, rec := range page.Records {
			records = append(records, toWireRecord(rec))
		}
		writeJSON(w, map[string]any{"records": records, "next": string(page.Next)})
	})
}

// verify recomputes the hash chain and reports whether it holds.
//
// The whole point of the evidence store is that this can be asked, so it is an
// endpoint rather than a CLI-only operation: a claim nobody can check from the
// page they are reading is a claim they have to take on trust.
func verify(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionEvidenceView)
		if !ok {
			return
		}
		q := r.URL.Query()
		proof, err := st.evidence.Verify(r.Context(), ref,
			evidence.Cursor(q.Get("from")), evidence.Cursor(q.Get("to")))
		if err != nil {
			problem(w, http.StatusInternalServerError, "zincir doğrulanamadı")
			return
		}
		// A broken chain is a 200 carrying valid=false, not a 5xx. It is an
		// answer the caller asked for and has to be able to render; a status
		// code that reads as "the server is broken" would send them to the
		// wrong place entirely.
		writeJSON(w, toWireProof(proof))
	})
}

// retention is what the store promises to keep, which the evidence page prints
// next to the history so that "nothing older than this" is visibly a policy
// rather than an absence of data.
func retention(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := g.admit(w, r, authz.ActionEvidenceView); !ok {
			return
		}
		policy, err := st.evidence.Retention(r.Context())
		if err != nil {
			problem(w, http.StatusInternalServerError, "saklama politikası okunamadı")
			return
		}
		writeJSON(w, map[string]any{
			"windowSeconds": int64(policy.Window.Seconds()),
			"keepCurrent":   policy.KeepCurrent,
			"immutable":     policy.Immutable,
			"anchor":        policy.Anchor,
		})
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already written; there is nothing left to say to the
		// client and nothing to do but let the connection close.
		return
	}
}

// export streams the evidence log for one app and environment.
//
// It writes the store's own encoding and not the wire types above, and that is
// deliberate. An export exists to be checked later — re-read, re-hashed, shown
// to somebody who was not there — so it has to carry the form the chain was
// computed over. The wire types are the opposite: a shape chosen to be
// convenient for a panel, which is free to change the day the panel does.
//
// The action is evidence:export rather than evidence:view. Reading one page in
// a browser and taking the whole log away are the same information at very
// different scales, and an install that wants the second restricted should not
// have to give up the first.
func export(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionEvidenceExport)
		if !ok {
			return
		}

		// Only what the store actually writes. csv is a declared format that
		// no store implements, and offering it here produced exactly the
		// failure the constant now warns about: a response headed text/csv,
		// named .csv, carrying JSONL.
		const format = evidence.ExportJSONL
		if got := r.URL.Query().Get("format"); got != "" && got != string(format) {
			problem(w, http.StatusBadRequest, "tek dışa aktarma biçimi jsonl")
			return
		}

		filename := "damga-" + ref.TenantID + "-" + ref.App + "-" + ref.Env + "." + string(format)
		w.Header().Set("Content-Type", "application/jsonl")
		// Quoted, because a tenant id or an app name with a space in it would
		// otherwise truncate the filename at the space.
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

		// Once Export writes its first byte the status is 200 and the headers
		// are gone. There is no way to turn a failure here into an error the
		// caller sees as an error, so it is logged and the response is left
		// truncated — which is at least detectable, unlike a 200 carrying a
		// short file that claims to be complete.
		//
		// Detectable because the last line of a JSONL export is a record whose
		// hash chains to the one before it: a truncated export fails to verify
		// at the point it was cut.
		if _, err := st.evidence.Export(r.Context(), evidence.ExportRequest{
			Query:  evidence.Query{Ref: ref, Order: evidence.OrderOldest},
			Format: format,
		}, w); err != nil {
			slog.Default().Error("the evidence export was cut short",
				"tenant", ref.TenantID, "app", ref.App, "env", ref.Env, "error", err)
		}
	})
}

// The three things this list can find, spelled out because the difference is
// the point rather than an inconsistency to smooth over.
const (
	// Placed and deployed to: the ordinary case.
	appDeployed = "deploy edildi"
	// Placed and never deployed to. This is what an app looks like between
	// POST /apps and the first deploy, and it is why this endpoint reads the
	// placement store at all.
	appNeverDeployed = "hiç deploy edilmedi"
	// Deployed to, and no longer placed. DELETE /apps/{app} removes the record
	// and not the app, so its history outlives its registration — and something
	// is running that the control plane can no longer deploy to.
	appRecordRemoved = "kaydı silindi"
)

// wireApp is one app environment as this list reports it.
type wireApp struct {
	App string `json:"app"`
	Env string `json:"env"`

	// State is the sentence the page renders, decided here rather than in the
	// panel — the same arrangement wireBackup uses, and for the same reason:
	// which of the two stores knows about an app is a fact about the platform,
	// not a rendering choice.
	State string `json:"state"`

	// The placement, when there is one. Omitted rather than empty for an app
	// only the deploy log remembers, so "there is no placement" and "the
	// placement has an empty path" are not the same JSON.
	RepoURL   string `json:"repoUrl,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Path      string `json:"path,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// apps lists this tenant's app environments, from both places that know about
// one, and says which of them knew.
//
// It used to answer from the evidence store alone, and the comment here argued
// that this was the honest answer because this is the evidence API. That
// argument stopped holding the moment POST /apps existed: an app that has been
// created and not yet deployed is exactly the app whose owner is looking for
// it, and answering "nothing here" made a working endpoint invisible — the
// user creates something, the page stays empty, and nothing anywhere says why.
//
// The two sources can disagree, and both disagreements are reported rather than
// reconciled. Placed with no history is an app waiting for its first deploy;
// history with no placement is one whose record was deleted while its objects
// and its log carry on. Neither is an error and neither is the other, so the
// list says which it is and lets the reader act on it.
func apps(g guard, st stores) http.Handler {
	type key struct{ app, env string }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionAppView)
		if !ok {
			return
		}

		placed, err := st.placement.List(r.Context(), ref.TenantID)
		if err != nil {
			problem(w, http.StatusInternalServerError, "yerleşim kayıtları okunamadı")
			return
		}
		refs, err := st.evidence.Refs(r.Context(), ref.TenantID)
		if err != nil {
			problem(w, http.StatusInternalServerError, "kanıt deposu okunamadı")
			return
		}

		merged := make(map[key]wireApp, len(placed)+len(refs))
		for _, p := range placed {
			merged[key{p.App, p.Env}] = wireApp{
				App: p.App, Env: p.Env, State: appNeverDeployed,
				RepoURL: p.RepoURL, Branch: p.Branch, Path: p.Path, Namespace: p.Namespace,
			}
		}
		for _, got := range refs {
			k := key{got.App, got.Env}
			if entry, placed := merged[k]; placed {
				entry.State = appDeployed
				merged[k] = entry
				continue
			}
			merged[k] = wireApp{App: got.App, Env: got.Env, State: appRecordRemoved}
		}

		out := make([]wireApp, 0, len(merged))
		for _, entry := range merged {
			out = append(out, entry)
		}
		// Map iteration is deliberately unordered in Go, and both sources
		// arrive sorted, so the merge has to sort or the panel's list would
		// reshuffle on every load.
		slices.SortFunc(out, func(a, b wireApp) int {
			if c := strings.Compare(a.App, b.App); c != 0 {
				return c
			}
			return strings.Compare(a.Env, b.Env)
		})
		writeJSON(w, map[string]any{"apps": out})
	})
}
