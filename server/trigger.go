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

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/placement"
)

// Connecting a source repository, which is the first of three links that were
// each built and never joined.
//
// placement.Store has carried SetTrigger and TriggersFor since before this
// file, all three stores implement them, and the webhook handler looks a push
// up through TriggersFor and creates a Build from what it finds. Nothing wrote
// a Trigger. So every part of `git push -> build -> deploy` existed and the
// chain was open at its first joint: a push arrived, matched nothing, and was
// answered with a 200 that meant "no trigger for this repository".
//
// # Where the secret goes, and why it is not where the 2026-09-04 decision put
// the other one
//
// That decision says a user's secret value goes into a Kubernetes Secret and
// git carries only the reference. This secret does not go there, and the
// difference is who needs to read it.
//
// An application's secret is read by the application, in the cluster, at run
// time — so the cluster is where it has to be. A webhook secret is read by this
// process, at the moment a push arrives, to verify an HMAC. Putting it in a
// Secret would mean the control plane reads a Secret on every webhook, which is
// a cluster round trip on an unauthenticated endpoint and a new grant
// (secrets/get) the ClusterRole deliberately does not hold — it holds create
// and patch precisely so it cannot read them back.
//
// So it lives in the placement store, in plaintext, and the Trigger type says
// why in as many words: an HMAC needs the key and not a hash of it. What
// follows from that is the rule this endpoint enforces — it is write-only.
// Nothing here, and nothing anywhere else, returns it.
func setTrigger(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionAppTrigger)
		if !ok {
			return
		}
		var req struct {
			Provider string `json:"provider"`
			RepoURL  string `json:"repoUrl"`
			Secret   string `json:"secret"`
		}
		if err := json.NewDecoder(
			http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "reading the request: "+err.Error())
			return
		}

		trigger, err := triggerFrom(ref, req.Provider, req.RepoURL, req.Secret)
		if err != nil {
			problem(w, http.StatusBadRequest, err.Error())
			return
		}

		// The placement has to exist first. A trigger pointing at an app with
		// no repository configured would accept a push and then fail at the
		// build with a message about manifests, three minutes and one webhook
		// later.
		if _, err := st.placement.Get(r.Context(), ref.TenantID, ref.App, ref.Env); err != nil {
			if errors.Is(err, placement.ErrNotFound) {
				problem(w, http.StatusNotFound,
					"this app and environment have no repository configured yet, so a push "+
						"to it would have nowhere to deploy")
				return
			}
			problem(w, http.StatusInternalServerError, "reading the placement failed")
			return
		}

		if err := st.placement.SetTrigger(r.Context(), trigger); err != nil {
			problem(w, http.StatusInternalServerError, "the trigger could not be saved")
			return
		}

		// The secret is not echoed, and this response is the shape that
		// guarantees it: there is no field for it to be returned in.
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]any{
			"provider":  trigger.Provider,
			"repoUrl":   trigger.RepoURL,
			"connected": true,
			// What to paste into the forge. The path is fixed and carries no
			// tenant, because the payload's own repository field is what
			// resolves the tenant — see the webhook handler.
			"deliverTo": "/hooks/" + trigger.Provider,
			"note": "the secret is stored to verify signatures and is never shown again. " +
				"Send it here again to replace it.",
		})
	})
}

// triggerFrom validates what a caller asked to connect.
func triggerFrom(ref evidence.Ref, provider, repoURL, secret string) (placement.Trigger, error) {
	provider = strings.TrimSpace(provider)
	repoURL = strings.TrimSpace(repoURL)

	switch {
	case provider == "":
		return placement.Trigger{}, errors.New(
			"a trigger needs the provider whose signatures it will check")
	case provider != providerGitHub:
		// Named rather than accepted-and-ignored. The webhook handler verifies
		// one scheme; a trigger stored for a forge nothing routes would sit
		// there matching nothing, which is the state this whole endpoint
		// exists to end.
		return placement.Trigger{}, errors.New(
			"the only provider this build understands is " + providerGitHub)
	case repoURL == "":
		return placement.Trigger{}, errors.New("a trigger needs the repository a push comes from")
	case secret == "":
		// Refused rather than defaulted to empty. An empty HMAC key verifies
		// any payload that was signed with an empty key, which is to say it
		// turns an authenticated endpoint into an open one.
		return placement.Trigger{}, errors.New(
			"a trigger needs a secret: without one the webhook would accept any payload " +
				"that claims to be from this repository")
	case len(secret) < 16:
		return placement.Trigger{}, errors.New(
			"the secret is shorter than 16 characters; it is the only thing standing " +
				"between a stranger and a build")
	}

	return placement.Trigger{
		TenantID: ref.TenantID, App: ref.App, Env: ref.Env,
		Provider: provider, RepoURL: repoURL, Secret: secret,
	}, nil
}
