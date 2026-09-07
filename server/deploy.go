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
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// deployRequest is what the panel sends. Only image is required: a deploy is
// usually "the same app, a new build", and everything left out keeps whatever
// is committed.
//
// Pointers for the optional fields, so "leave it alone" and "set it to zero"
// are different requests. Without that, a form that does not send a replica
// count and a form that asks for zero replicas are the same bytes, and one of
// those two meanings is "take the app down".
type deployRequest struct {
	Image    string  `json:"image"`
	Port     *int32  `json:"port,omitempty"`
	Replicas *int32  `json:"replicas,omitempty"`
	Domain   *string `json:"domain,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// deploy commits a new desired state for one app environment.
//
// It writes to git and to nothing else. Nothing here touches the cluster: Argo
// CD applies what was committed, admission is the last gate, and the record
// this opens is closed by the observer watching what actually happened. That
// is why the response is a pending record rather than a success — at this
// point the only thing that is true is that a commit was pushed.
// deployRoute is the table-shaped entry point; deploy is what it is.
func deployRoute(g guard, st stores) http.Handler {
	return deploy(g, st.writer, st.placement, st.gitAuth)
}

func deploy(
	g guard, w *gitwrite.Writer, places placement.Store,
	auth GitAuth,
) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		sub, ref, ok := g.admit(rw, r, authz.ActionAppDeploy)
		if !ok {
			return
		}

		var req deployRequest
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&req); err != nil {
			problem(rw, http.StatusBadRequest, "istek gövdesi beklenen JSON değil")
			return
		}
		if req.Image == "" {
			problem(rw, http.StatusBadRequest, "bir deploy için imaj gerekir")
			return
		}

		place, err := places.Get(r.Context(), ref.TenantID, ref.App, ref.Env)
		switch {
		case errors.Is(err, placement.ErrNotFound):
			// Not a 404 on the deploy: the deploy is fine, there is nowhere to
			// put it. Saying which makes the difference between "you typed the
			// wrong app" and "this app has never been configured".
			problem(rw, http.StatusConflict,
				"bu uygulama ve ortam için henüz bir depo tanımlı değil")
			return
		case err != nil:
			problem(rw, http.StatusInternalServerError, "yerleşim kaydı okunamadı")
			return
		}

		method, err := auth.For(place.RepoURL)
		if err != nil {
			problem(rw, http.StatusInternalServerError, err.Error())
			return
		}

		result, err := w.Deploy(r.Context(), gitwrite.Request{
			Target: gitwrite.Target{
				RepoURL: place.RepoURL, Branch: place.Branch, Dir: place.Path, Auth: method,
			},
			// The commit author is the person, from the session. The committer
			// is the platform; gitwrite sets that. What is written here is the
			// instance-local audit alias and never the login address, because
			// a commit cannot be redacted.
			Author: gitwrite.Author{ID: sub.ID, Name: sub.ID, Email: sub.Email},
			Ref:    ref,
			// Recorded here rather than left to the observer. The observer
			// writes what it finds running; this writes what was asked for, and
			// an install with -observe-deploys off has only the second.
			Image:   req.Image,
			Message: fmt.Sprintf("deploy %s to %s/%s", req.Image, ref.App, ref.Env),
			Render:  renderDeploy(place, req),
		})
		switch {
		case errors.Is(err, gitwrite.ErrNoChange):
			// A redeploy of something identical. Not an error and not a
			// commit: inventing an empty one would put a deploy in the history
			// that changed nothing.
			problem(rw, http.StatusConflict, "commit edilmiş olan zaten bu")
			return
		case err != nil:
			problem(rw, http.StatusBadGateway, "commit push edilemedi: "+err.Error())
			return
		}

		rw.WriteHeader(http.StatusAccepted)
		writeJSON(rw, toWireRecord(result.Record))
	})
}

// renderFunc is gitwrite's render callback, named so the signature fits on a
// line and so there is one place to change if it grows.
type renderFunc = func(rolloutID string, current map[string][]byte) (map[string][]byte, error)

// renderDeploy applies the request to whatever is already committed.
//
// Read-modify-write against git, with no second copy of the spec in the
// control plane's database. The committed file is the state, so the panel and
// the cluster cannot drift apart — and a field the request does not mention
// keeps the value somebody set last time rather than the type's zero.
// The signature policy travels with the manifest it protects.
//
// Written into the same directory, by the same path, in the same commit — which
// makes it desired state like everything else here rather than something a
// second reconciler applies out of band. Three things follow from that and each
// is the reason not to do it another way: damga stays the only writer, so the
// rule that admits an image is under the same authorship as the image; the
// evidence record covers the policy as well as the workload, so "what was
// enforcing when this deployed" is answerable; and the policy is rendered in
// exactly one place, so it cannot drift from the workflow it pins the way a
// second renderer in the operator would.
func renderDeploy(place placement.Placement, req deployRequest) renderFunc {
	return func(rolloutID string, current map[string][]byte) (map[string][]byte, error) {
		app := platformv1alpha1.Workload{}
		if body, ok := current[manifest.File]; ok {
			parsed, err := manifest.Parse(body)
			if err != nil {
				// Refused rather than overwritten. Something is in the
				// directory that this build cannot read, and replacing it
				// would delete whatever it is.
				return nil, fmt.Errorf("commit edilmiş manifest okunamıyor: %w", err)
			}
			app = parsed
		}

		// Identity comes from the placement every time, not from the file. A
		// file that names a different namespace is one somebody moved without
		// telling the control plane, and following it would deploy into
		// whatever it says.
		app.ObjectMeta = metav1.ObjectMeta{
			Name: place.App, Namespace: place.Namespace, Annotations: app.Annotations,
		}

		app.Spec.Image = req.Image
		if req.Port != nil {
			app.Spec.Port = *req.Port
		}
		if req.Replicas != nil {
			app.Spec.Replicas = req.Replicas
		}
		if req.Domain != nil {
			app.Spec.Domain = *req.Domain
		}

		body, err := manifest.Render(app, rolloutID)
		if err != nil {
			return nil, err
		}
		// The fence travels with every commit, not only with an install: an app
		// created through POST /apps and deployed has no other way to get one,
		// and a workload that lands in a namespace with no quota is the failure
		// this platform already paid for from the other end.
		//
		// Rendered rather than carried forward, because these two files are
		// core/v1 and manifest.Owns recognises only this platform's own group —
		// so nothing would keep them, and nothing would remove them either.
		files, err := manifest.Fence(place.Namespace)
		if err != nil {
			return nil, err
		}
		files[manifest.File] = body
		return carryForward(files, current), nil
	}
}

// carryForward adds every other manifest this platform wrote in the directory,
// byte for byte.
//
// A placement's directory holds several objects now: a catalogue entry with a
// worker and a database installs as three manifests, each in its own file. The
// renders in this package all change exactly one of them — a deploy is "this
// app, a new image", a scale is a replica count — and have no opinion at all
// about the rest.
//
// Returning them unchanged rather than omitting them is what makes that
// harmless, because omission is how this platform asks gitwrite for a file to
// be removed. A render that forgets a sibling is a deploy that deletes a
// database the moment its request carries an Owns rule, and the install's does.
// It is one function rather than a loop in each render so that adding a render
// is adding a call rather than remembering a rule.
//
// It is also why none of these renders sets Owns itself: a render that never
// stops producing a file it owns has nothing for a deletion rule to act on.
func carryForward(out, current map[string][]byte) map[string][]byte {
	for name, existing := range current {
		if _, mine := out[name]; mine || !manifest.Owns(existing) {
			continue
		}
		out[name] = existing
	}
	return out
}
