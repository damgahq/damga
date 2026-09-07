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
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/placement"
)

// maxRequestBody bounds every request body this package decodes. Without it a
// client that announces a small JSON object and then sends gigabytes is a way
// to spend the server's memory from an unauthenticated-adjacent position — the
// guard runs first, but a member of any tenant is still not somebody who should
// be able to do that.
const maxRequestBody = 64 << 10

// createAppRequest brings one app environment into existence.
//
// Every field is required and none is guessed, which is the opposite of the
// deploy request one file over. That asymmetry is deliberate: a deploy is "the
// same app, a new build" and leaving a field out means keeping what is
// committed, while this is the moment there is nothing to keep. A default here
// would be the platform inventing where a tenant's code lives, and the two
// values it would have to invent are the two this repository has already
// written down reasons not to invent — a branch nobody is watching looks
// exactly like a deploy that worked, and a namespace derived from an identity
// makes a rename into a rewrite.
type createAppRequest struct {
	App string `json:"app"`
	Env string `json:"env"`

	RepoURL string `json:"repoUrl"`
	Branch  string `json:"branch"`
	Path    string `json:"path"`

	Namespace string `json:"namespace"`
}

// wirePlacement is what an app looks like on the wire.
//
// Its own type for the reason wire.go gives at length: what a placement is in
// the database is a storage concern, and what an HTTP response looks like is a
// product surface that will change. This one carries no secret — credentials
// are deliberately not on a placement — so it is the whole row minus its
// tenant, which the caller supplied in the path and does not need read back.
type wirePlacement struct {
	App string `json:"app"`
	Env string `json:"env"`

	RepoURL string `json:"repoUrl"`
	Branch  string `json:"branch"`
	Path    string `json:"path"`

	Namespace string `json:"namespace"`

	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

func toWirePlacement(p placement.Placement) wirePlacement {
	return wirePlacement{
		App: p.App, Env: p.Env,
		RepoURL: p.RepoURL, Branch: p.Branch, Path: p.Path,
		Namespace: p.Namespace,
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// createApp records where one app environment lives, which is the first link
// in the chain and was the missing one.
//
// Nothing wrote to the placement store before this endpoint. GET /apps listed
// what had been deployed, POST .../deploys refused with "this app and
// environment have no repository configured yet", and there was no way to
// configure one that did not involve an INSERT by hand. Every other endpoint in
// the table reads or extends something this one has to create first.
//
// It writes to the control plane's database and to nothing else. No commit, no
// cluster: a placement is where a deploy will go, not a deploy. The first
// commit is still made by the first deploy, which is what keeps the git write
// path the only thing that ever produces a manifest.
func createApp(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// env:create rather than app:deploy. Creating an environment is what
		// the closed action set already calls this, and it is the weaker of the
		// two rights: a member may bring an app into existence, a viewer may
		// not, and neither fact should depend on whether they may also ship an
		// image to it.
		_, ref, ok := g.admit(w, r, authz.ActionEnvCreate)
		if !ok {
			return
		}

		var req createAppRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "istek gövdesi beklenen JSON değil")
			return
		}

		// Trimmed before anything is checked. Leading or trailing space in a
		// name that becomes a Kubernetes object or a directory is never what
		// the sender meant, and a browser form supplies it for free — the
		// alternative is refusing " api" with a message about DNS labels that
		// points at a character nobody can see.
		req.App, req.Env = strings.TrimSpace(req.App), strings.TrimSpace(req.Env)
		req.Namespace = strings.TrimSpace(req.Namespace)
		req.RepoURL, req.Branch = strings.TrimSpace(req.RepoURL), strings.TrimSpace(req.Branch)
		req.Path = strings.TrimSpace(req.Path)

		// The three names that become Kubernetes objects are checked here and
		// not left to the store, because the store's job is the invariants and
		// this is a shape. An app called "My API" is accepted by every layer
		// between here and the cluster and then rejected by the API server, at
		// which point the failure is a rollout that never appears rather than a
		// sentence the person who typed it can read.
		for _, f := range []struct{ what, value string }{
			{"uygulama adı", req.App},
			{"ortam", req.Env},
			{fieldNamespace, req.Namespace},
		} {
			if f.value == "" {
				problem(w, http.StatusBadRequest, "bir uygulama için uygulama adı, ortam ve namespace gerekir")
				return
			}
			if msgs := validation.IsDNS1123Label(f.value); len(msgs) > 0 {
				problem(w, http.StatusBadRequest, f.what+" Kubernetes'te kullanılamaz: "+msgs[0])
				return
			}
		}
		// The repository is not checked for a scheme here. GitAuth is what
		// decides which schemes an install can push to — the free build's
		// answer is https only, and an in-cluster git server reached over a
		// private network needs none — so a rule here would either duplicate
		// that answer or contradict it.
		if req.RepoURL == "" {
			problem(w, http.StatusBadRequest, "bir uygulama için manifest'lerinin yazılacağı bir depo gerekir")
			return
		}

		// Advisory, and it is worth saying which half is not. Two creates of
		// the same app racing each other both see nothing here and both write,
		// and the store's Put is create-or-replace, so the result is one row
		// carrying whichever arrived second rather than a duplicate. What this
		// check buys is the ordinary case — an app that already exists is a
		// 409 that says so, instead of a silent repointing of a live app at
		// another repository. The cross-tenant case is not defended here at
		// all; that is the store's claim, below.
		if _, err := st.placement.Get(r.Context(), ref.TenantID, req.App, req.Env); err == nil {
			problem(w, http.StatusConflict, "bu uygulama ve ortam zaten var")
			return
		} else if !errors.Is(err, placement.ErrNotFound) {
			problem(w, http.StatusInternalServerError, "yerleşim kaydı okunamadı")
			return
		}

		got, err := st.placement.Put(r.Context(), placement.Placement{
			// The tenant comes from the path the guard already checked, never
			// from the body. A tenant a caller could name in JSON is an
			// authorization that was performed against a different tenant from
			// the one that gets written.
			TenantID:  ref.TenantID,
			App:       req.App,
			Env:       req.Env,
			RepoURL:   req.RepoURL,
			Branch:    req.Branch,
			Path:      req.Path,
			Namespace: req.Namespace,
		})
		switch {
		case errors.Is(err, placement.ErrInvalid):
			// The store's own words. They name which field and why — "path %q
			// is not a directory inside the repository" — and every value they
			// quote came from this request, so there is nothing here that the
			// caller did not already send.
			problem(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, placement.ErrConflict):
			// A repository or a namespace another tenant holds. The message
			// names the value, which the caller sent, and never the tenant that
			// holds it — the same reason the guard's refusal does not confirm
			// that a tenant exists.
			problem(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "yerleşim kaydı yazılamadı")
			return
		}

		// The Application is created with the placement rather than with the
		// first deploy, so the commit a deploy makes has something already
		// watching for it. Argo CD polls, so one that appears afterwards still
		// finds the commit — it just waits for a poll rather than a sync.
		delivered := "bir Argo CD Application bu uygulamanın dizinini izliyor"
		note, err := deliverPlacement(r.Context(), st, got)
		switch {
		case err != nil:
			delivered = "bu uygulamanın commit'lerini henüz hiçbir şey uygulamıyor: " + err.Error()
		case note != "":
			delivered += ", ancak " + note
		}

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"app": toWirePlacement(got), keyDelivery: delivered})
	})
}

// deleteApp forgets where an app lives. It deletes the record and not the app.
//
// Nothing here touches git, the cluster or the evidence log, and that is the
// whole shape of it. The manifests stay committed, whatever Argo CD applied
// keeps running, the database keeps its data and its backups, and the deploy
// history stays readable — GET /apps is drawn from the evidence store, so an
// app deleted here still appears there with everything that ever happened to
// it. What is gone is the platform's answer to "where would the next deploy go",
// which is why the next deploy refuses with "no repository configured yet"
// rather than writing somewhere it guessed.
//
// The response lists what was removed for exactly that reason: after this call
// there are objects running in a namespace and files in a repository that
// nothing in the control plane points at any more, and the caller is the last
// party in a position to be told where they are.
func deleteApp(g guard, st stores) http.Handler {
	type removed struct {
		Env       string `json:"env"`
		RepoURL   string `json:"repoUrl"`
		Path      string `json:"path"`
		Namespace string `json:"namespace"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// app:delete, which the free authorizer answers member and above.
		//
		// This was tenant:admin — owner-only — for exactly as long as the action
		// did not exist. That was the conservative end of a decision nobody had
		// made, and it was wrong in the safe direction: a member may create an
		// app and ship to it, and there is no story in which the same person
		// must then find an owner to unregister one. The action set is closed so
		// that adding an endpoint forces this decision rather than letting it be
		// borrowed from a neighbour, and this is the decision.
		_, ref, ok := g.admit(w, r, authz.ActionAppDelete)
		if !ok {
			return
		}

		all, err := st.placement.List(r.Context(), ref.TenantID)
		if err != nil {
			problem(w, http.StatusInternalServerError, "yerleşim kayıtları okunamadı")
			return
		}
		// Every environment of this app, because the path names an app and not
		// an environment. Listed first and deleted afterwards rather than
		// deleted as they are found, so that a store that fails halfway is one
		// this handler noticed rather than one it reported success from.
		out := make([]removed, 0, len(all))
		for _, p := range all {
			if p.App != ref.App {
				continue
			}
			out = append(out, removed{
				Env: p.Env, RepoURL: p.RepoURL, Path: p.Path, Namespace: p.Namespace,
			})
		}
		if len(out) == 0 {
			problem(w, http.StatusNotFound, "bu kiracının bu adda bir uygulaması yok")
			return
		}

		for _, p := range out {
			if err := st.placement.Delete(r.Context(), ref.TenantID, ref.App, p.Env); err != nil {
				// Deletes are not one transaction: the store deletes one
				// placement at a time and there is no batch. Stopping at the
				// first failure leaves the earlier environments deleted, which
				// is why this says how many rather than claiming nothing
				// happened — and why repeating the call is safe, since deleting
				// what is not there is not an error.
				problem(w, http.StatusInternalServerError,
					"uygulama yalnızca kısmen kaldırıldı: "+p.Env+" ve ondan sonrakiler hâlâ kayıtlı")
				return
			}
		}
		writeJSON(w, map[string]any{"app": ref.App, "removed": out})
	})
}
