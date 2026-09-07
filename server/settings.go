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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
const settingsSecretNote = "Bu değer git'te değil. Git yalnızca adı ve hangi Secret'ın içinde " +
	"yaşadığını taşır; değerin kendisi doğrudan kümeye yazılır, yani onun için " +
	"\"kim ne zaman değiştirdi\" yoktur, geri alma yoktur, ve Secret silinirse hiçbir şey " +
	"onu geri koymaz — Argo CD'nin yaklaşık on saniyede geri getirdiği bir manifest'in aksine."

// buildNotConsumedWarning is returned for every build-time variable, because
// one is recorded and delivered nowhere.
//
// Measured rather than assumed: BuildSpec carries repo, revision, path, image,
// builder and resources, and no environment at all, and internal/controller's
// build job injects none. A setting that is stored and does nothing is the
// exact thing this endpoint exists to stop being invisible, so it says so in
// the same breath as accepting it.
const buildNotConsumedWarning = "derleme zamanı değişkenleri kaydediliyor ve henüz onları " +
	"kullanan bir şey yok: bir Build hiçbir ortam taşımıyor, yani değer hiçbir derlemeye ulaşmıyor. " +
	"Kullanılana kadar ayarın kaybolmaması için saklanıyor."

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
			problem(w, http.StatusBadRequest, "istek gövdesi beklenen JSON değil")
			return
		}

		app, place, ok := readCommittedWorkload(w, r, st, ref)
		if !ok {
			return
		}
		if app.Spec.Image == "" {
			problem(w, http.StatusConflict,
				"buraya henüz hiçbir şey deploy edilmedi, yani yapılandırılacak bir şey yok")
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
					"bu kurulum bir sır değeri tutamıyor: Secret'ın yazılacağı bir küme olmadan "+
						"başlatıldı, bu yüzden yalnızca düz değerler ayarlanabilir")
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
				problemWritingSecrets(w, place.Namespace, err)
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
				problem(w, http.StatusConflict, "commit edilmiş olan zaten bu")
				return
			}
		case err != nil:
			problem(w, http.StatusBadGateway, "commit push edilemedi: "+err.Error())
			return
		}

		// After the commit. Until it lands, git still names these variables,
		// and a value removed before its name is a pod that cannot start.
		if len(plan.remove) > 0 {
			if err := st.secrets.Put(r.Context(), place.Namespace, secretName, nil, plan.remove); err != nil {
				problem(w, http.StatusBadGateway,
					"ayarlar commit edildi ve kaldırılan sır değerleri hâlâ kümede: "+
						err.Error())
				return
			}
		}

		warnings := settingsWarnings(plan.app)
		if len(plan.set) > 0 {
			// Said on the save that wrote a value rather than only on the page
			// that shows it. The commit below records that the settings changed
			// and who changed them; what it does not contain is the value, so
			// "committed" on its own would promise a rollback that is not there.
			warnings = append(warnings, "bir sır değeri commit'te yer almaz: git bu uygulamanın "+
				"ayarlarının değiştiğini, kimin ne zaman değiştirdiğini kaydeder, ve değerin "+
				"kendisi yalnızca kümede bulunur")
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
				"kaydedildi: sır değerleri değişti ve hiçbir şey commit edilmedi, çünkü git "+
					"onları taşımıyor")
		}
		w.WriteHeader(code)
		writeJSON(w, body)
	})
}

// problemWritingSecrets turns the API server's refusal into the sentence that
// names what is actually wrong.
//
// Forbidden is the interesting one and it has exactly one ordinary cause. The
// control plane holds no cluster-wide permission over Secrets: what lets it
// write into a tenant's namespace is a Role and a RoleBinding that live in the
// tenant's own repository, beside the namespace and the quota, and arrive when
// Argo CD applies them. So a refusal here almost always means the manifests are
// committed and have not been applied yet — either the first sync of a new app,
// or a namespace that predates this arrangement and has not been redeployed
// since.
//
// Answered as a conflict rather than a 502, because nothing is broken: the state
// is committed, the cluster is behind it, and pressing save again after the sync
// works. A 502 would send somebody to look at the control plane's logs for a
// fault that is not there.
func problemWritingSecrets(w http.ResponseWriter, namespace string, err error) {
	if apierrors.IsForbidden(err) {
		problem(w, http.StatusConflict,
			"bu kurulum henüz "+namespace+" içine yazamaz: orada bir sır değeri tutma izni, bu "+
				"uygulamanın kendi dizinindeki bir manifest'tir, ve Argo CD onu henüz "+
				"uygulamadı. Bu uygulamayı bir kez deploy edin ya da bir sonraki sync'i "+
				"bekleyin, sonra yeniden kaydedin. Düz ayarlar bundan etkilenmez.")
		return
	}
	problem(w, http.StatusBadGateway, "sır değerleri yazılamadı: "+err.Error())
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
		problem(w, http.StatusNotFound, "bu uygulama ve ortam için henüz bir depo tanımlı değil")
		return platformv1alpha1.Workload{}, place, false
	case err != nil:
		problem(w, http.StatusInternalServerError, "yerleşim kaydı okunamadı")
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
		problem(w, http.StatusBadGateway, "durum deposu okunamadı: "+err.Error())
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
		problem(w, http.StatusBadGateway, "commit edilmiş manifest okunamıyor: "+err.Error())
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
			"bir uygulama %d ortam değişkeni taşıyabilir, bu istek ise %d tane istiyor",
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
			return settingsPlan{}, errors.New("bir değişkenin adı olmalı")
		case !envNamePattern.MatchString(key):
			return settingsPlan{}, fmt.Errorf(
				"%q kullanılabilir bir ortam değişkeni adı değil: bir harfle ya da alt çizgiyle "+
					"başlamalı ve yalnızca harf, rakam ve alt çizgi taşımalı, yoksa konteyner "+
					"o değişken olmadan başlar ve bunu söyleyen hiçbir şey olmaz", key)
		case len(key) > 128:
			return settingsPlan{}, fmt.Errorf("%q adı 128 karakterden uzun", key)
		case seen[key]:
			return settingsPlan{}, fmt.Errorf(
				"%s listede iki kez var; bir değişkenin bir değeri olur", key)
		case !v.Build && !v.Runtime:
			return settingsPlan{}, fmt.Errorf(
				"%s ne derleme zamanı ne de çalışma zamanı değişkeni, yani onu teslim edecek "+
					"bir yer de saklayacak bir yer de yok", key)
		case v.Secret && v.Build:
			// Not a missing field. Builds run in their own namespace and a
			// Secret in the tenant's is not reachable from there, so the value
			// would have to be copied into the build namespace by something
			// that can read it — and nothing in this platform can read a
			// secret, deliberately.
			return settingsPlan{}, fmt.Errorf(
				"%s derleme zamanı sırrı olamaz: derlemeler kendi namespace'lerinde çalışır ve "+
					"buradaki hiçbir şey oraya bir değer kopyalayamaz", key)
		case v.Secret && v.Value != nil && *v.Value == "":
			return settingsPlan{}, fmt.Errorf(
				"değeri boş olan bir sır, sır değildir: %s değişkenini kaldırmak için listeden "+
					"çıkarın ya da bir değer gönderin", key)
		case !v.Secret && v.Value == nil:
			return settingsPlan{}, fmt.Errorf(
				"%s bir değer taşımıyor: bir değer gönderin, boş olması gerekiyorsa \"\" gönderin", key)
		}
		seen[key] = true

		switch {
		case v.Secret:
			if v.Value == nil && !slices.Contains(current.Spec.UserSecrets, key) {
				return settingsPlan{}, fmt.Errorf(
					"%s yeni ve bir değeri yok: hiç ayarlanmamış bir sır korunamaz", key)
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
			return out, fmt.Errorf("%s yolu / ile başlamalı", p.what)
		}
	}
	for _, f := range []struct {
		what      string
		v, lo, hi int32
	}{
		{"sağlık portu", out.Port, 1, 65535},
		{"prob aralığı", out.IntervalSeconds, 1, 300},
		{"prob zaman aşımı", out.TimeoutSeconds, 1, 60},
		{"hata eşiği", out.FailureThreshold, 1, 20},
	} {
		if f.v != 0 && (f.v < f.lo || f.v > f.hi) {
			return out, fmt.Errorf("%s %d ile %d arasında olmalı", f.what, f.lo, f.hi)
		}
	}
	if out.TimeoutSeconds != 0 && out.IntervalSeconds != 0 && out.TimeoutSeconds > out.IntervalSeconds {
		// The API server would refuse this after the commit, with the CRD's own
		// words and a commit already pushed.
		return out, errors.New(
			"prob zaman aşımı, problar arasındaki aralıktan uzun olamaz")
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
		{"CPU isteği", r.CPURequest, &out.CPURequest},
		{"bellek isteği", r.MemoryRequest, &out.MemoryRequest},
		{"bellek limiti", r.MemoryLimit, &out.MemoryLimit},
	} {
		if strings.TrimSpace(f.in) == "" {
			continue
		}
		q, err := resource.ParseQuantity(f.in)
		if err != nil {
			return out, fmt.Errorf("%s olarak verilen %q bir miktar değil: 100m ve 512Mi birer miktar", f.what, f.in)
		}
		if q.Sign() <= 0 {
			return out, fmt.Errorf("%s sıfırdan büyük olmalı", f.what)
		}
		*f.field = q
	}
	if !out.MemoryLimit.IsZero() && !out.MemoryRequest.IsZero() &&
		out.MemoryLimit.Cmp(out.MemoryRequest) < 0 {
		// The kubelet refuses the pod, so this would be a commit that applies
		// and never runs.
		return out, fmt.Errorf("bellek limiti (%s) bellek isteğinin (%s) altında",
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
		out = append(out, buildNotConsumedWarning)
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
			return nil, fmt.Errorf("commit edilmiş manifest okunamıyor: %w", err)
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
