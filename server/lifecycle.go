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
	"errors"
	"fmt"
	"net/http"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// The refusals a render can produce. They travel out through gitwrite, which
// wraps the render's error with %w, and are matched with errors.Is rather than
// echoed — the wrapped text says "rendering the manifests", which is true and
// is not what the person who pressed the button needs to read.
var (
	errNothingDeployed = errors.New("server: nothing is deployed here yet")

	// A fixed replica count and an autoscaler are mutually exclusive in the
	// CRD ("set replicas or autoscale, not both"). Without this the commit
	// lands, Argo CD tries to apply it, and the API server refuses it — so the
	// scale looks like it worked and the app stops syncing.
	errAutoscaled = errors.New("server: the replica count belongs to the autoscaler")

	// A workload with volumes is pinned to one replica by the operator, which
	// overrides spec.replicas rather than refusing it. Nothing rejects the
	// commit, so a scale to five would report success and change nothing.
	errPinnedByVolumes = errors.New("server: a workload with a volume runs one replica")

	// The two ways a deploy number resolves to nothing.
	errNoSuchDeploy = errors.New("server: no deploy with that number")
	errTooFarBack   = errors.New("server: that deploy is beyond the scan")
)

const (
	// rollbackPage is how many records one scan step reads. 200 and not more,
	// because the two stores disagree above it: the in-memory store treats a
	// limit over 200 as unset and quietly gives 50, while the SQL one accepts
	// up to 500. A page size both honour is one whose behaviour does not depend
	// on which engine an install chose.
	rollbackPage = 200

	// maxRollbackScan bounds the walk backwards through history.
	//
	// A bound and not a measurement: this runs inside an HTTP request, and
	// evidence.Query has no way to ask for one sequence number, so reaching
	// deploy 3 of an app with 40,000 deploys means reading 40,000 rows. The
	// walk stops early in every ordinary case — history is newest-first and
	// sequence numbers are gapless, so a rollback to something recent reads one
	// page — and this is only reached by asking for something very old.
	maxRollbackScan = 2000
)

// rollbackRoute redeploys the image a previous deploy ran.
//
// # A rollback is a new commit
//
// It is not a git history rewrite and not a revert, and the difference is not
// stylistic. Argo CD tracks a branch: pointing it at an older revision, or
// force-pushing the branch back, puts selfHeal in a fight with whoever moved it
// — the cluster is reconciled towards what the branch says, and the branch is
// what was just changed underneath it. Principle 1 says the write path is git,
// and this is what respecting it looks like: the old image is committed forward
// as a new desired state, with a new evidence record naming who asked.
//
// # What it restores, and what it does not
//
// The image, and only the image. Port, domain, replica count and everything
// else keep whatever is committed now.
//
// That is a limit of what is recorded rather than a choice: an evidence record
// carries the image, the commit, the actor and the outcome — it has never
// carried the manifest. The full manifest at that commit does exist, in the
// state repository, but gitwrite reads the working tree at HEAD and has no way
// to read a file at an arbitrary revision. Restoring the whole spec needs that
// capability in internal/gitwrite, which this change does not own.
//
// The narrower behaviour is also the one most platforms mean by rollback, and
// it is the one the record was designed for — docs/PLAN.md calls the deploy log
// "geri almanın dayanağı", the basis of a rollback, and lists what it holds:
// who, when, which commit, which image, what became of it.
func rollbackRoute(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ref, ok := g.admit(w, r, authz.ActionAppRollback)
		if !ok {
			return
		}

		seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
		if err != nil || seq < 1 {
			problem(w, http.StatusBadRequest, "deploy numarası pozitif bir tam sayıdır")
			return
		}

		target, err := recordBySeq(r.Context(), st.evidence, ref, seq)
		switch {
		case errors.Is(err, errNoSuchDeploy):
			problem(w, http.StatusNotFound,
				fmt.Sprintf("bu uygulama ve ortamda %d numaralı deploy yok", seq))
			return
		case errors.Is(err, errTooFarBack):
			// Not a 404: the deploy may well exist. Saying which is the
			// difference between "you typed the wrong number" and "this build
			// cannot reach that far back", and only the second is fixed by
			// giving evidence.Query a way to ask for one sequence number.
			problem(w, http.StatusNotImplemented,
				fmt.Sprintf("%d numaralı deploy %d deploy'dan daha geride kaldı; bu kurulum o kadar geriye ulaşamıyor",
					seq, maxRollbackScan))
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "kanıt deposu okunamadı")
			return
		}

		switch target.State {
		case evidence.StateRejected, evidence.StateFailed:
			// Only the two states that mean it definitely never ran. Pending,
			// syncing and unknown are all "nobody saw what happened", and an
			// install with ObserveDeploys off leaves every record in one of
			// them — refusing those would make rollback unusable on exactly
			// the installs whose control plane is outside the cluster.
			problem(w, http.StatusConflict,
				fmt.Sprintf("%d numaralı deploy hiç çalışmadı: kayıtta durumu %s", seq, target.State))
			return
		}
		if target.Image.RequestedRef == "" {
			problem(w, http.StatusConflict,
				fmt.Sprintf("%d numaralı deploy hiçbir imaj kaydetmemiş, yani geri getirilecek bir şey yok", seq))
			return
		}

		commitChange(w, r, st, sub, ref,
			fmt.Sprintf("roll %s/%s back to deploy %d (%s)",
				ref.App, ref.Env, seq, target.Image.RequestedRef),
			func(place placement.Placement) renderFunc {
				// The deploy renderer, with only the image set. Every optional
				// field left nil means "keep what is committed", which is the
				// whole of what a rollback wants — and reusing it is what stops
				// a second renderer from drifting away from the first.
				return renderDeploy(place, deployRequest{Image: target.Image.RequestedRef})
			},
			commitOptions{})
	})
}

// scaleRequest is how many replicas to run.
//
// A pointer, so that a body with no count and a body asking for zero are
// different requests. Zero is refused with a reason rather than read as "not
// sent", because "take the app down" is a thing somebody will type.
type scaleRequest struct {
	Replicas *int32 `json:"replicas"`
}

// scaleRoute changes how many replicas an app runs.
//
// Through git, like everything else: spec.replicas on the committed Workload,
// which the operator renders onto the Deployment. Nothing here touches the
// cluster, and it does not need to — this is one of the two lifecycle
// operations that is already expressible as desired state.
//
// Two states of the committed manifest make a scale a lie, and both are refused
// before the commit rather than discovered afterwards:
//
//   - An autoscaled workload. The CRD refuses replicas and autoscale together,
//     so the commit would be pushed and then rejected by the API server on
//     every sync — a scale that reports success and stops the app syncing.
//   - A workload with a volume. The operator pins it to one replica because its
//     storage is ReadWriteOnce, and it does that by overriding the field rather
//     than refusing it. Nothing would reject the commit; the count would simply
//     never take effect.
func scaleRoute(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ref, ok := g.admit(w, r, authz.ActionAppScale)
		if !ok {
			return
		}

		var req scaleRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "istek gövdesi beklenen JSON değil")
			return
		}
		if req.Replicas == nil {
			problem(w, http.StatusBadRequest, "ölçekleme için bir replika sayısı gerekiyor")
			return
		}
		if *req.Replicas < 1 {
			// The CRD's own floor, mirrored here so the refusal is a sentence
			// rather than a CEL rule quoted back. Scaling to zero is a real
			// product wish and is not this endpoint: the type has no way to
			// express a stopped app, and inventing one here would put it in
			// half the platform.
			problem(w, http.StatusBadRequest,
				"bir workload en az bir replika çalıştırır; bu uç bir uygulamayı durduramaz")
			return
		}

		replicas := *req.Replicas
		commitChange(w, r, st, sub, ref,
			fmt.Sprintf("scale %s/%s to %d replicas", ref.App, ref.Env, replicas),
			func(place placement.Placement) renderFunc { return renderScale(place, replicas) },
			commitOptions{})
	})
}

// restartRoute would replace an app's pods with the same image. It cannot, and
// it says so rather than doing something adjacent.
//
// # Why this is 501 and not a commit
//
// A restart is a change to the pod template. That is the whole mechanism:
// `kubectl rollout restart` writes an annotation into
// `.spec.template.metadata.annotations`, the Deployment controller sees a new
// pod template hash, and a new ReplicaSet replaces the old pods. A change
// anywhere else on the Deployment — including its own annotations — creates no
// ReplicaSet and restarts nothing.
//
// The only write path is git, so a restart would have to be a field on the
// Workload that reaches the pod template. Read in
// internal/controller/resources.go: it does not exist. The Deployment's
// ObjectMeta gets the platform's annotations (`withAnnotations(objectMeta(app),
// platformAnnotations(app))`), and the pod template's ObjectMeta is
// `metav1.ObjectMeta{Labels: labelsFor(app)}` — labels, and nothing else. The
// observer confirms the arrangement from the other side: it reads
// `damga.co/rollout` off the Deployment's own annotations, not off the template.
//
// The alternatives were considered and each is worse than an honest refusal:
//
//   - Writing a marker into spec.env. It reaches the pod template, and it is
//     not inert — it is an environment variable the application can read, and
//     it would stay in the manifest for ever.
//   - Scaling to zero and back. The CRD's floor is one replica, and it is an
//     outage rather than a restart.
//   - Deleting pods from the control plane. There is no such permission and
//     that is deliberate: cluster/control-plane.yaml grants read cluster-wide
//     and creates nothing in a tenant namespace. A platform that can delete a
//     tenant's pods is a platform whose compromise is a tenant's outage.
//
// What would make this work is one line in the operator: carry the platform's
// annotations onto the pod template as well as onto the Deployment, or give the
// Workload a field whose only job is to change the template. Both are in
// internal/controller, which this change does not own.
func restartRoute(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionAppRestart)
		if !ok {
			return
		}
		// Asked first, so that "there is no such app here" and "this platform
		// cannot restart anything" are different answers. Without it every
		// typo in an app name reads as a missing feature.
		if _, err := st.placement.Get(r.Context(), ref.TenantID, ref.App, ref.Env); err != nil {
			if errors.Is(err, placement.ErrNotFound) {
				problem(w, http.StatusNotFound,
					"bu uygulama ve ortam için henüz bir depo tanımlı değil")
				return
			}
			problem(w, http.StatusInternalServerError, "yerleşim kaydı okunamadı")
			return
		}
		problem(w, http.StatusNotImplemented,
			"bu kurulum bir uygulamayı yeniden başlatamıyor: yeniden başlatma pod şablonunda bir "+
				"değişikliktir, tek yazma yolu git'tir, ve bir Workload'ın hiçbir alanı pod şablonuna ulaşmaz")
	})
}

// commitChange is the shared tail of every lifecycle write: find where the app
// lives, get the credential for it, commit, and answer with the record.
//
// It is here rather than in deploy.go because deploy.go is not this change's to
// edit; the deploy handler still carries its own copy of this shape and could
// use this one.
//
// The render callback takes the placement because the placement is what decides
// the object's name and namespace, and it is not known until the lookup below
// has happened.
func commitChange(
	w http.ResponseWriter, r *http.Request, st stores,
	sub authz.Subject, ref evidence.Ref, message string,
	render func(placement.Placement) renderFunc,
	opts commitOptions,
) {
	place, err := st.placement.Get(r.Context(), ref.TenantID, ref.App, ref.Env)
	switch {
	case errors.Is(err, placement.ErrNotFound):
		problem(w, http.StatusConflict,
			"bu uygulama ve ortam için henüz bir depo tanımlı değil")
		return
	case err != nil:
		problem(w, http.StatusInternalServerError, "yerleşim kaydı okunamadı")
		return
	}

	method, err := st.gitAuth.For(place.RepoURL)
	if err != nil {
		problem(w, http.StatusInternalServerError, err.Error())
		return
	}

	result, err := st.writer.Deploy(r.Context(), gitwrite.Request{
		Target: gitwrite.Target{
			RepoURL: place.RepoURL, Branch: place.Branch, Dir: place.Path, Auth: method,
		},
		// The author is the person from the session, and the instance-local
		// audit alias rather than the login address — a commit cannot be
		// redacted. The committer is the platform; gitwrite sets that.
		Author:  gitwrite.Author{ID: sub.ID, Name: sub.ID, Email: sub.Email},
		Ref:     ref,
		Message: message,
		Render:  render(place),
		Owns:    owns(opts.MayRemove),
	})
	switch {
	case errors.Is(err, gitwrite.ErrNoChange):
		problem(w, http.StatusConflict, "commit edilmiş olan zaten bu")
		return
	case errors.Is(err, errNothingDeployed):
		problem(w, http.StatusConflict,
			"buraya henüz hiçbir şey deploy edilmedi, yani değiştirilecek bir şey yok")
		return
	case errors.Is(err, errAutoscaled):
		problem(w, http.StatusConflict,
			"bu uygulama otomatik ölçekleniyor, yani replika sayısı otomatik ölçekleyicinin; "+
				"onun yerine otomatik ölçekleme aralığını değiştirin")
		return
	case errors.Is(err, errDatabaseExists):
		problem(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, errNoSuchDatabase):
		problem(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, errLastManifest):
		problem(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, errPinnedByVolumes):
		problem(w, http.StatusConflict,
			"bu uygulamanın kalıcı bir volume'ü var, bu yüzden tam olarak bir replika çalıştırıyor: "+
				"deposu aynı anda tek bir node tarafından mount edilebilir")
		return
	case err != nil:
		problem(w, http.StatusBadGateway, "commit push edilemedi: "+err.Error())
		return
	}

	// Accepted, like a deploy: a commit was pushed and that is all that is true
	// yet. What the cluster did with it is the observer's to say.
	w.WriteHeader(http.StatusAccepted)
	if opts.Note == "" {
		writeJSON(w, toWireRecord(result.Record))
		return
	}
	writeJSON(w, map[string]any{"record": toWireRecord(result.Record), "note": opts.Note})
}

// commitOptions is what a caller wants that is not the render.
//
// A struct rather than more parameters, because both fields are the kind that
// change what a commit does and neither should be a bare true in a call.
type commitOptions struct {
	// Note is said alongside the record. It exists for one route: removing a
	// database is a commit whose consequences for the data are not in the
	// commit, and the moment to say so is the response to the request that
	// asked for it. See deleteDatabase.
	Note string

	// MayRemove lets gitwrite delete files this platform wrote that the render
	// stopped producing.
	//
	// Off by default and per caller, which is gitwrite's own rule rather than
	// this file's: "a caller that has not thought about deletion therefore does
	// not get it". A rollback and a scale rewrite one manifest and never
	// withdraw one, so they must not gain the ability to; removing a database
	// is nothing but a withdrawal, and without this it commits a change that
	// gitwrite then discards — which surfaced as "this is already what is
	// committed" rather than as anything about deletion.
	MayRemove bool
}

// owns answers gitwrite's per-file question, or declines to.
func owns(mayRemove bool) func(string, []byte) bool {
	if !mayRemove {
		return nil
	}
	// By content and not by name, which is what manifest.Owns is for: a
	// filename is a convention, and recognising somebody else's file as ours
	// removes work from a repository they cannot push to.
	return func(_ string, body []byte) bool { return manifest.Owns(body) }
}

// renderScale changes the replica count and leaves everything else committed.
func renderScale(place placement.Placement, replicas int32) renderFunc {
	return func(rolloutID string, current map[string][]byte) (map[string][]byte, error) {
		body, ok := current[manifest.File]
		if !ok {
			// A placement with no manifest: the app was created and never
			// deployed. Scaling it would render a Workload out of nothing,
			// which fails deeper down on the missing image with a message
			// about manifests rather than about this app.
			return nil, errNothingDeployed
		}
		app, err := manifest.Parse(body)
		if err != nil {
			return nil, fmt.Errorf("commit edilmiş manifest okunamıyor: %w", err)
		}
		if app.Spec.Autoscale != nil {
			return nil, errAutoscaled
		}
		if len(app.Spec.Volumes) > 0 {
			return nil, errPinnedByVolumes
		}

		// Identity from the placement and never from the file, for the reason
		// renderDeploy gives: a file naming a different namespace is one
		// somebody moved without telling the control plane.
		app.ObjectMeta = metav1.ObjectMeta{
			Name: place.App, Namespace: place.Namespace, Annotations: app.Annotations,
		}
		app.Spec.Replicas = &replicas

		out, err := manifest.Render(app, rolloutID)
		if err != nil {
			return nil, err
		}
		// The other objects in the directory, untouched: a scale is a replica
		// count for one workload and says nothing about the database beside
		// it. See carryForward for why omitting them is a deletion.
		return carryForward(map[string][]byte{manifest.File: out}, current), nil
	}
}

// recordBySeq walks the deploy log backwards until it reaches one sequence
// number.
//
// A walk because there is no other way to ask: evidence.Query filters by ref,
// state, actor and time, and a Seq is the one thing the panel actually shows a
// person. Adding it to the Query is a change to evidence/, which this change
// does not own — with it, this function is one indexed lookup.
//
// The walk is cheap in the case that happens. History comes back newest-first
// and sequence numbers are per-ref monotonic and gapless, so rolling back to
// the deploy before last reads one page and stops on the second record; and
// seeing any record older than the target proves the target does not exist,
// which ends the walk without reading to the beginning of the log.
func recordBySeq(
	ctx context.Context, store evidence.Store, ref evidence.Ref, seq int64,
) (evidence.Record, error) {
	var (
		after   evidence.Cursor
		scanned int
	)
	for {
		page, err := store.History(ctx, evidence.Query{
			Ref: ref, Limit: rollbackPage, After: after, Order: evidence.OrderNewest,
		})
		if err != nil {
			return evidence.Record{}, err
		}
		if len(page.Records) == 0 {
			return evidence.Record{}, errNoSuchDeploy
		}
		for _, rec := range page.Records {
			switch {
			case rec.Seq == seq:
				return rec, nil
			case rec.Seq < seq:
				return evidence.Record{}, errNoSuchDeploy
			}
		}
		scanned += len(page.Records)
		if page.Next == "" {
			return evidence.Record{}, errNoSuchDeploy
		}
		if scanned >= maxRollbackScan {
			return evidence.Record{}, errTooFarBack
		}
		after = page.Next
	}
}
