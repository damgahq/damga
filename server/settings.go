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
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// The settings endpoint: what an application is configured with, and the one
// door the product did not have.
//
// # Why this exists
//
// n8n does not install from the catalogue, and the reason is not a bug. Its
// template shares one token between two variables, the platform mints one value
// per variable, and there was no way for a person to type a value at all. The
// same absence keeps every application that needs a database address or an API
// key out of the catalogue. Coolify has a text box here; this had a refusal.
//
// # Where a value goes, and why there are two answers
//
// A literal is committed: it goes in the Workload's Env, in git, in the diff,
// and anyone who can read the repository can read it. A secret is not: the
// value is written straight into a Secret beside the workload and git carries
// only the name of the variable and which Secret it lives in.
//
// That split is decided in docs/KARARLAR.md (2026-09-04) and it is bought with
// something real. A value that is not in git has no history and no rollback,
// and if the Secret is deleted nothing puts it back — measured on 2026-09-02,
// a manifest comes back in about ten seconds and a directly written object does
// not come back at all. secretNote is that sentence, returned to whoever is
// about to type a value rather than left in a document.

// settingsSecretNote is what the panel prints beside a secret. It is returned
// rather than left to the page, so the control plane and the panel cannot end
// up telling a user two different things about the same value.
const settingsSecretNote = "This value is not in git. Git carries the name and which Secret it " +
	"lives in; the value itself is written straight into the cluster, so there is no " +
	"\"who changed it and when\" for it, no rollback, and if the Secret is deleted nothing " +
	"puts it back — unlike a manifest, which Argo CD restores in about ten seconds."

// buildTimeWarning is returned for every build-time variable, and it is not the
// one that used to be here.
//
// That one said the value was recorded and consumed by nothing, which was true
// and measured: BuildSpec carried no environment at all. It does now, and both
// injection paths were measured — buildkit takes --opt build-arg:, the
// lifecycle takes a file per variable under the platform directory — so the
// honest sentence is a different one.
//
// This is the thing somebody actually gets wrong next: setting a build-time
// variable and looking at the running container for it. It reaches the build
// that produces the image, and nothing about the container that is already
// running changes until something builds and deploys.
const buildTimeWarning = "a build-time variable reaches the next build of this app, not the " +
	"container that is running now: it is given to the build that produces the image, so " +
	"nothing changes until this app is built and deployed again."

// envNamePattern is what envFrom will actually deliver.
//
// A Secret may hold a key like "my.key" and a workload may not read it: envFrom
// skips a key that is not a valid environment variable name, the pod starts,
// the variable is absent, and the only trace is an event nobody reads. Refused
// here, where there is somebody to tell.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// maxSettingsEnv bounds the list. The CRD carries the same number, and the
// reason is the same in both places: a committed manifest is an object the API
// server holds, and an unbounded list is an unbounded object.
const maxSettingsEnv = 64

// wireSecretRef is where a secret value actually lives, which is the thing git
// can show and a platform holding values in its own database cannot.
type wireSecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// wireEnvVar is one variable.
//
// Value is a pointer and its absence is load-bearing in both directions. On the
// way out, a secret carries no value at all — not "" — because "" is a value an
// application can read and telling the two apart is the difference between "not
// shown" and "set to nothing". On the way in, absence on a secret means "leave
// it as it is", which is what lets a form be saved without retyping every
// password on the page.
type wireEnvVar struct {
	Key       string         `json:"key"`
	Value     *string        `json:"value,omitempty"`
	Secret    bool           `json:"secret,omitempty"`
	Build     bool           `json:"build"`
	Runtime   bool           `json:"runtime"`
	SecretRef *wireSecretRef `json:"secretRef,omitempty"`
}

type wireHealth struct {
	LivenessPath     string `json:"livenessPath"`
	ReadinessPath    string `json:"readinessPath"`
	Port             int32  `json:"port,omitempty"`
	IntervalSeconds  int32  `json:"intervalSeconds,omitempty"`
	TimeoutSeconds   int32  `json:"timeoutSeconds,omitempty"`
	FailureThreshold int32  `json:"failureThreshold,omitempty"`
}

// wireResources has no CPU limit and that is not an omission.
//
// The CRD has none either: CPU is compressible, so throttling a container is
// better than the latency cliff a limit produces, while memory is not, so
// exceeding it has to mean eviction. A field here that nothing reads would be a
// box somebody types into and a setting that silently does nothing.
type wireResources struct {
	CPURequest    string `json:"cpuRequest"`
	MemoryRequest string `json:"memoryRequest"`
	MemoryLimit   string `json:"memoryLimit"`
}

type wireSettings struct {
	Env       []wireEnvVar  `json:"env"`
	Health    wireHealth    `json:"health"`
	Resources wireResources `json:"resources"`
}

// SecretWriter puts the values a user typed into the cluster.
//
// A seam for the reason Deliverer is one: an install with no cluster still has
// to start, serve the panel and say what it cannot do. nil means a literal can
// be set and a secret cannot, and the endpoint says which rather than writing
// half a setting.
//
// Set and remove rather than a whole map, because this interface is deliberately
// built on top of a permission the control plane does not have. It may create
// and patch a Secret and may not read one, so it cannot know the values it is
// keeping — a merge patch changes the keys it names and leaves the rest, which
// is the only shape that works without the read.
type SecretWriter interface {
	Put(ctx context.Context, namespace, name string, set map[string]string, remove []string) error
}

// settingsRoute answers what an app is configured with.
func settingsRoute(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionAppView)
		if !ok {
			return
		}
		app, _, ok := readCommittedWorkload(w, r, st, ref)
		if !ok {
			return
		}
		out := toWireSettings(app)
		writeJSON(w, map[string]any{
			"env": out.Env, "health": out.Health, "resources": out.Resources,
			"warnings":   settingsWarnings(app),
			"secretNote": settingsSecretNote,
		})
	})
}

// updateSettingsRoute writes them.
//
// A full replace, which is what PUT means and is also the only shape that can
// express a deletion: this endpoint has no "remove" verb, so a variable that is
// not in the list is one that is gone. The alternative — a PATCH with an
// explicit clear — puts a "wipe this secret" instruction on the wire, where a
// form that forgot to send a value can produce it by accident.
func updateSettingsRoute(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ref, ok := g.admit(w, r, authz.ActionAppSettings)
		if !ok {
			return
		}

		var req wireSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "the request body is not the expected JSON")
			return
		}

		app, place, ok := readCommittedWorkload(w, r, st, ref)
		if !ok {
			return
		}
		if app.Spec.Image == "" {
			problem(w, http.StatusConflict,
				"nothing is deployed here yet, so there is nothing to configure")
			return
		}

		plan, err := planSettings(req, app)
		if err != nil {
			problem(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(plan.set) > 0 || len(plan.remove) > 0 {
			if st.secrets == nil {
				problem(w, http.StatusConflict,
					"this installation cannot hold a secret value: it was started without a "+
						"cluster to write the Secret into, so only literal values can be set")
				return
			}
		}

		// The values first, and the removals last, so that git never references
		// a key that is not there yet and never keeps a value it no longer
		// names. A commit that fails after the values are written leaves values
		// nothing reads, which costs nothing; the other order leaves a workload
		// whose pod cannot start.
		secretName := manifest.UserSecretName(place.App)
		if len(plan.set) > 0 {
			if err := st.secrets.Put(r.Context(), place.Namespace, secretName, plan.set, nil); err != nil {
				problem(w, http.StatusBadGateway, "the secret values could not be written: "+err.Error())
				return
			}
		}

		result, err := commitSettings(r.Context(), st, sub, ref, place, plan)
		switch {
		case errors.Is(err, gitwrite.ErrNoChange):
			// Unreachable today, and kept for the reason commitChange keeps the
			// same mapping: manifest.Render stamps a fresh damga.co/rollout
			// annotation on every render, so the bytes always differ and the
			// worktree is never clean. Measured in
			// TestARollbackToWhatIsAlreadyRunningStillCommits.
			//
			// If that ever changes, this is the branch that has to exist: a
			// secret value that changed produces no diff, because git never
			// carries one, and answering "this is already what is committed"
			// would be a lie about a change that really happened.
			if len(plan.set) == 0 {
				problem(w, http.StatusConflict, "this is already what is committed")
				return
			}
		case err != nil:
			problem(w, http.StatusBadGateway, "the commit could not be pushed: "+err.Error())
			return
		}

		// After the commit. Until it lands, git still names these variables,
		// and a value removed before its name is a pod that cannot start.
		if len(plan.remove) > 0 {
			if err := st.secrets.Put(r.Context(), place.Namespace, secretName, nil, plan.remove); err != nil {
				problem(w, http.StatusBadGateway,
					"the settings were committed and the removed secret values are still in "+
						"the cluster: "+err.Error())
				return
			}
		}

		warnings := settingsWarnings(plan.app)
		if len(plan.set) > 0 {
			// Said on the save that wrote a value rather than only on the page
			// that shows it. The commit below records that the settings changed
			// and who changed them; what it does not contain is the value, so
			// "committed" on its own would promise a rollback that is not there.
			warnings = append(warnings, "a secret value is not in the commit: git records "+
				"that this app's settings changed, who changed them and when, and the value "+
				"itself exists only in the cluster")
		}
		body := map[string]any{
			"settings":   toWireSettings(plan.app),
			"warnings":   warnings,
			"secretNote": settingsSecretNote,
			"record":     nil,
		}
		code := http.StatusOK
		if result != nil {
			body["record"] = toWireRecord(*result)
			code = http.StatusAccepted
		} else {
			body["warnings"] = append(warnings,
				"saved: the secret values changed and nothing was committed, because git "+
					"does not carry them")
		}
		w.WriteHeader(code)
		writeJSON(w, body)
	})
}

// readCommittedWorkload is the state, read from the only place it lives.
//
// There is no second copy in the control plane's database. That is what keeps
// the panel and the cluster from drifting apart, and it is why this reads git
// rather than the live object: the committed file is what will be applied
// again on the next sync, and the live object is what happened last time.
func readCommittedWorkload(
	w http.ResponseWriter, r *http.Request, st stores, ref evidence.Ref,
) (platformv1alpha1.Workload, placement.Placement, bool) {
	place, err := st.placement.Get(r.Context(), ref.TenantID, ref.App, ref.Env)
	switch {
	case errors.Is(err, placement.ErrNotFound):
		problem(w, http.StatusNotFound, "this app and environment have no repository configured yet")
		return platformv1alpha1.Workload{}, place, false
	case err != nil:
		problem(w, http.StatusInternalServerError, "reading the placement failed")
		return platformv1alpha1.Workload{}, place, false
	}

	method, err := st.gitAuth.For(place.RepoURL)
	if err != nil {
		problem(w, http.StatusInternalServerError, err.Error())
		return platformv1alpha1.Workload{}, place, false
	}
	files, err := st.writer.Read(r.Context(), gitwrite.Target{
		RepoURL: place.RepoURL, Branch: place.Branch, Dir: place.Path, Auth: method,
	})
	if err != nil {
		problem(w, http.StatusBadGateway, "the state repository could not be read: "+err.Error())
		return platformv1alpha1.Workload{}, place, false
	}

	body, ok := files[manifest.File]
	if !ok {
		// An app that has been registered and never deployed. Not an error:
		// the settings of nothing are empty, and the page that shows them is
		// the page somebody is about to use.
		return platformv1alpha1.Workload{}, place, true
	}
	app, err := manifest.Parse(body)
	if err != nil {
		problem(w, http.StatusBadGateway, "the committed manifest cannot be read: "+err.Error())
		return platformv1alpha1.Workload{}, place, false
	}
	return app, place, true
}

// toWireSettings is what the page draws.
//
// Secrets carry their name, the Secret they live in and no value, and there is
// no code path here that could produce one: the control plane holds create and
// patch on Secrets and not get, so it has never seen the value it is not
// returning.
func toWireSettings(app platformv1alpha1.Workload) wireSettings {
	out := wireSettings{Env: make([]wireEnvVar, 0, len(app.Spec.Env))}
	for _, e := range app.Spec.Env {
		value := e.Value
		out.Env = append(out.Env, wireEnvVar{Key: e.Name, Value: &value, Runtime: true})
	}
	secretName := manifest.UserSecretName(app.Name)
	for _, name := range app.Spec.UserSecrets {
		out.Env = append(out.Env, wireEnvVar{
			Key: name, Secret: true, Runtime: true,
			SecretRef: &wireSecretRef{Name: secretName, Key: name},
		})
	}
	for _, e := range app.Spec.BuildEnv {
		value := e.Value
		if at := indexOfKey(out.Env, e.Name); at >= 0 {
			// The same variable on both sides. One row with both boxes ticked,
			// because that is one variable in the eyes of whoever set it — and
			// two rows with the same name is a screen nobody can act on.
			out.Env[at].Build = true
			continue
		}
		out.Env = append(out.Env, wireEnvVar{Key: e.Name, Value: &value, Build: true})
	}
	slices.SortFunc(out.Env, func(a, b wireEnvVar) int { return strings.Compare(a.Key, b.Key) })

	out.Health = wireHealth{
		LivenessPath:     app.Spec.Health.LivenessPath,
		ReadinessPath:    app.Spec.Health.ReadinessPath,
		Port:             app.Spec.Health.Port,
		IntervalSeconds:  app.Spec.Health.IntervalSeconds,
		TimeoutSeconds:   app.Spec.Health.TimeoutSeconds,
		FailureThreshold: app.Spec.Health.FailureThreshold,
	}
	out.Resources = wireResources{
		CPURequest:    quantityString(app.Spec.Resources.CPURequest),
		MemoryRequest: quantityString(app.Spec.Resources.MemoryRequest),
		MemoryLimit:   quantityString(app.Spec.Resources.MemoryLimit),
	}
	return out
}

func indexOfKey(list []wireEnvVar, key string) int {
	return slices.IndexFunc(list, func(v wireEnvVar) bool { return v.Key == key })
}

// quantityString avoids "0" for a value nobody set. An unset quantity is the
// platform's default, which the CRD fills in, and printing a zero here would
// show a workload asking for no memory.
func quantityString(q resource.Quantity) string {
	if q.IsZero() {
		return ""
	}
	return q.String()
}

// settingsPlan is the whole of what a PUT decided, worked out before anything
// is written.
type settingsPlan struct {
	app    platformv1alpha1.Workload
	set    map[string]string
	remove []string
}

// planSettings turns the request into the manifest to commit and the two
// changes to make to the Secret, and refuses everything it cannot do.
//
// Every refusal here is a sentence about one field, because that is what
// somebody typing into a form can act on. They are worked out before the first
// write for the same reason a plan is: half of these settings applied is a
// worse state than none of them.
func planSettings(req wireSettings, current platformv1alpha1.Workload) (settingsPlan, error) {
	if len(req.Env) > maxSettingsEnv {
		return settingsPlan{}, fmt.Errorf(
			"an app may carry %d environment variables and this asks for %d",
			maxSettingsEnv, len(req.Env))
	}

	app := *current.DeepCopy()
	app.Spec.Env = nil
	app.Spec.UserSecrets = nil
	app.Spec.BuildEnv = nil

	plan := settingsPlan{set: map[string]string{}}
	seen := map[string]bool{}
	for _, v := range req.Env {
		key := strings.TrimSpace(v.Key)
		switch {
		case key == "":
			return settingsPlan{}, errors.New("a variable needs a name")
		case !envNamePattern.MatchString(key):
			return settingsPlan{}, fmt.Errorf(
				"%q is not a usable environment variable name: it has to start with a letter "+
					"or an underscore and hold only letters, digits and underscores, or the "+
					"container is started without it and nothing says so", key)
		case len(key) > 128:
			return settingsPlan{}, fmt.Errorf("the name %q is longer than 128 characters", key)
		case seen[key]:
			return settingsPlan{}, fmt.Errorf(
				"%s is listed twice; one variable has one value", key)
		case !v.Build && !v.Runtime:
			return settingsPlan{}, fmt.Errorf(
				"%s is neither a build-time nor a run-time variable, so there is nowhere to "+
					"deliver it and nowhere to keep it", key)
		case v.Secret && v.Build:
			// Not a missing field. Builds run in their own namespace and a
			// Secret in the tenant's is not reachable from there, so the value
			// would have to be copied into the build namespace by something
			// that can read it — and nothing in this platform can read a
			// secret, deliberately.
			return settingsPlan{}, fmt.Errorf(
				"%s cannot be a build-time secret: builds run in their own namespace and "+
					"nothing here can copy a value into it", key)
		case v.Secret && v.Value != nil && *v.Value == "":
			return settingsPlan{}, fmt.Errorf(
				"a secret with an empty value is not a secret: leave %s out of the list to "+
					"remove it, or send a value", key)
		case !v.Secret && v.Value == nil:
			return settingsPlan{}, fmt.Errorf(
				"%s has no value: send one, and send \"\" if that is what it should be", key)
		}
		seen[key] = true

		switch {
		case v.Secret:
			if v.Value == nil && !slices.Contains(current.Spec.UserSecrets, key) {
				return settingsPlan{}, fmt.Errorf(
					"%s is new and has no value: a secret that was never set cannot be kept", key)
			}
			app.Spec.UserSecrets = append(app.Spec.UserSecrets, key)
			if v.Value != nil {
				plan.set[key] = *v.Value
			}
		default:
			if v.Runtime {
				app.Spec.Env = append(app.Spec.Env,
					platformv1alpha1.EnvVar{Name: key, Value: *v.Value})
			}
			if v.Build {
				app.Spec.BuildEnv = append(app.Spec.BuildEnv,
					platformv1alpha1.EnvVar{Name: key, Value: *v.Value})
			}
		}
	}

	// Gone from the list is gone from the Secret. The manifest stops naming it
	// in the same commit, so nothing is left referencing a key that is not
	// there.
	for _, was := range current.Spec.UserSecrets {
		if !seen[was] {
			plan.remove = append(plan.remove, was)
		}
	}

	health, err := planHealth(req.Health)
	if err != nil {
		return settingsPlan{}, err
	}
	app.Spec.Health = health

	res, err := planResources(req.Resources, current.Spec.Resources)
	if err != nil {
		return settingsPlan{}, err
	}
	app.Spec.Resources = res

	plan.app = app
	return plan, nil
}

// planHealth mirrors the CRD's bounds so the refusal is a sentence rather than
// a CEL rule quoted back by the API server after the commit.
func planHealth(h wireHealth) (platformv1alpha1.Health, error) {
	out := platformv1alpha1.Health{
		LivenessPath: h.LivenessPath, ReadinessPath: h.ReadinessPath,
		Port: h.Port, IntervalSeconds: h.IntervalSeconds,
		TimeoutSeconds: h.TimeoutSeconds, FailureThreshold: h.FailureThreshold,
	}
	for _, p := range []struct {
		what, path string
	}{{"liveness", out.LivenessPath}, {"readiness", out.ReadinessPath}} {
		if p.path != "" && !strings.HasPrefix(p.path, "/") {
			return out, fmt.Errorf("the %s path has to start with /", p.what)
		}
	}
	for _, f := range []struct {
		what      string
		v, lo, hi int32
	}{
		{"health port", out.Port, 1, 65535},
		{"probe interval", out.IntervalSeconds, 1, 300},
		{"probe timeout", out.TimeoutSeconds, 1, 60},
		{"failure threshold", out.FailureThreshold, 1, 20},
	} {
		if f.v != 0 && (f.v < f.lo || f.v > f.hi) {
			return out, fmt.Errorf("the %s has to be between %d and %d", f.what, f.lo, f.hi)
		}
	}
	if out.TimeoutSeconds != 0 && out.IntervalSeconds != 0 && out.TimeoutSeconds > out.IntervalSeconds {
		// The API server would refuse this after the commit, with the CRD's own
		// words and a commit already pushed.
		return out, errors.New(
			"the probe timeout cannot be longer than the interval between probes")
	}
	return out, nil
}

func planResources(r wireResources, current platformv1alpha1.Resources) (platformv1alpha1.Resources, error) {
	out := current
	for _, f := range []struct {
		what  string
		in    string
		field *resource.Quantity
	}{
		{"CPU request", r.CPURequest, &out.CPURequest},
		{"memory request", r.MemoryRequest, &out.MemoryRequest},
		{"memory limit", r.MemoryLimit, &out.MemoryLimit},
	} {
		if strings.TrimSpace(f.in) == "" {
			continue
		}
		q, err := resource.ParseQuantity(f.in)
		if err != nil {
			return out, fmt.Errorf("the %s %q is not a quantity: 100m and 512Mi are", f.what, f.in)
		}
		if q.Sign() <= 0 {
			return out, fmt.Errorf("the %s has to be more than zero", f.what)
		}
		*f.field = q
	}
	if !out.MemoryLimit.IsZero() && !out.MemoryRequest.IsZero() &&
		out.MemoryLimit.Cmp(out.MemoryRequest) < 0 {
		// The kubelet refuses the pod, so this would be a commit that applies
		// and never runs.
		return out, fmt.Errorf("the memory limit (%s) is below the memory request (%s)",
			out.MemoryLimit.String(), out.MemoryRequest.String())
	}
	return out, nil
}

// settingsWarnings is what was accepted and does less than it looks like.
//
// Separate from a refusal on purpose, and the difference is the whole point: a
// refusal is a save that did not happen, and a warning is a save that happened
// and delivers nothing yet.
func settingsWarnings(app platformv1alpha1.Workload) []string {
	out := []string{}
	if len(app.Spec.BuildEnv) > 0 {
		out = append(out, buildTimeWarning)
	}
	return out
}

// commitSettings writes the manifest, and returns nil when there was nothing to
// write.
func commitSettings(
	ctx context.Context, st stores, sub authz.Subject, ref evidence.Ref,
	place placement.Placement, plan settingsPlan,
) (*evidence.Record, error) {
	method, err := st.gitAuth.For(place.RepoURL)
	if err != nil {
		return nil, err
	}
	result, err := st.writer.Deploy(ctx, gitwrite.Request{
		Target: gitwrite.Target{
			RepoURL: place.RepoURL, Branch: place.Branch, Dir: place.Path, Auth: method,
		},
		Author:  gitwrite.Author{ID: sub.ID, Name: sub.ID, Email: sub.Email},
		Ref:     ref,
		Image:   plan.app.Spec.Image,
		Message: fmt.Sprintf("settings %s/%s", ref.App, ref.Env),
		Render:  renderSettings(place, plan.app),
	})
	if err != nil {
		return nil, err
	}
	return &result.Record, nil
}

// renderSettings writes the settings onto whatever else is committed.
func renderSettings(place placement.Placement, want platformv1alpha1.Workload) renderFunc {
	return func(rolloutID string, current map[string][]byte) (map[string][]byte, error) {
		body, ok := current[manifest.File]
		if !ok {
			return nil, errNothingDeployed
		}
		app, err := manifest.Parse(body)
		if err != nil {
			return nil, fmt.Errorf("the committed manifest cannot be read: %w", err)
		}

		// The settings, and only the settings. Everything else on the spec is
		// whatever the last deploy left — this endpoint changes what an app is
		// configured with and has no opinion about which image it runs.
		app.Spec.Env = want.Spec.Env
		app.Spec.UserSecrets = want.Spec.UserSecrets
		app.Spec.BuildEnv = want.Spec.BuildEnv
		app.Spec.Health = want.Spec.Health
		app.Spec.Resources = want.Spec.Resources

		// Identity from the placement and never from the file, for the reason
		// renderDeploy gives.
		app.ObjectMeta = metav1.ObjectMeta{
			Name: place.App, Namespace: place.Namespace, Annotations: app.Annotations,
		}

		out, err := manifest.Render(app, rolloutID)
		if err != nil {
			return nil, err
		}
		files, err := manifest.Fence(place.Namespace)
		if err != nil {
			return nil, err
		}
		files[manifest.File] = out
		return carryForward(files, current), nil
	}
}
