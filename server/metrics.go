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
	"log/slog"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/placement"
)

// AppMetrics is what one app environment looks like right now.
//
// # What this is not
//
// It is a snapshot and not a time series, and that is a scope decision rather
// than a first version. History needs something that stores it, and the two
// candidates — Prometheus, or a table of our own — are both a second system to
// operate for a question this endpoint does not ask. Measured before choosing:
// scripts/install.sh installs neither Prometheus nor kube-state-metrics, and CI
// installs neither, so a time series would have been built on a component the
// product does not ship.
//
// It also carries no request rate and no latency. Those come from the
// application's own /metrics, which means the application has to expose one and
// something has to scrape it — a general observability platform, which
// docs/PLAN.md puts outside the boundary in as many words. What is inside it is
// the user seeing whether their own app is healthy, and the three things that
// answer that are here: is it up, is it being restarted, and is it running out
// of memory.
type AppMetrics struct {
	Pods []PodSample

	// UsageSource names what answered for CPU and memory, and is empty when
	// nothing did.
	//
	// Empty is a real answer rather than a failure: pod facts come from the
	// Kubernetes API, which is always there, and usage comes from
	// metrics.k8s.io, which is a separate component an install may not have.
	// Reporting zeros instead would be a number, and a wrong one.
	UsageSource string

	// UsageNote says why usage is missing when it is, naming the component
	// rather than the symptom.
	UsageNote string
}

// PodSample is one replica.
type PodSample struct {
	Name      string
	Phase     string
	Ready     bool
	Restarts  int32
	StartedAt time.Time

	// LastTerminated is why the previous container of this pod ended, when
	// there was one.
	//
	// The field this endpoint exists for. A restart count says something is
	// wrong; OOMKilled says what, and it is the failure a platform user is
	// least equipped to diagnose from the outside — the pod is Running, the
	// Deployment is Available, and the application has been dying every four
	// minutes since the last deploy.
	LastTerminated *Termination

	CPU    Sample
	Memory Sample
}

// Termination is how a container ended.
type Termination struct {
	Reason   string
	ExitCode int32
	At       time.Time
}

// Sample is one resource: what the container is using, and what it was promised.
//
// Usage alone is a number nobody can act on. 312Mi is fine against a 512Mi
// limit and an outage against 320Mi, so the two travel together or neither is
// worth showing.
type Sample struct {
	Usage   *resource.Quantity
	Request *resource.Quantity
	Limit   *resource.Quantity
}

// MetricsSource reads one app's health from the cluster.
//
// A seam for the reason LogSource and BackupReader are seams: this is another
// place the control plane reads the cluster, and an install with no cluster to
// read has to start, serve, and say so rather than report an app with no pods.
type MetricsSource interface {
	ForApp(ctx context.Context, namespace, app string) (AppMetrics, error)
}

// appMetrics answers what one app environment is doing.
//
// # Why this GET carries an Origin check
//
// The same exception logs documents. run.go wraps everything in
// http.CrossOriginProtection and that control always allows GET, because a safe
// method changes nothing. What comes back here is one tenant's own operational
// state — replica names, restart counts, how close to its limit it is running —
// so a page on another origin that can make a browser ask, with the session
// cookie attached, reads it. The exemption is wrong here for the same reason
// and is not inherited.
func appMetrics(g guard, st stores) http.Handler {
	// Resolved once, like the log source and for the same reason: building a
	// client reads a token and a CA bundle off disk, and a panel that polls
	// this would pay for it on every poll.
	return appMetricsFrom(g, st, sync.OnceValues(inClusterMetrics))
}

// appMetricsFrom is appMetrics with the source injected, which is how a test
// drives it without a cluster.
func appMetricsFrom(g guard, st stores, open func() (MetricsSource, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := sameOrigin(r); err != nil {
			problem(w, http.StatusForbidden, "çapraz kaynaklı istek reddedildi")
			return
		}
		// app:view. Reading whether your own application is healthy is the
		// weakest thing this API offers, and a role that may look at an app
		// must be able to see why it is restarting.
		_, ref, ok := g.admit(w, r, authz.ActionAppView)
		if !ok {
			return
		}

		place, err := st.placement.Get(r.Context(), ref.TenantID, ref.App, ref.Env)
		switch {
		case errors.Is(err, placement.ErrNotFound):
			problem(w, http.StatusNotFound, "bu uygulama ve ortam için henüz bir depo tanımlı değil")
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "yerleşim kaydı okunamadı")
			return
		}

		src, err := open()
		if err != nil {
			slog.Warn("app metrics are unavailable", "error", err)
			problem(w, http.StatusNotImplemented,
				"bu kurulum kümeden pod durumunu okuyamıyor")
			return
		}

		got, err := src.ForApp(r.Context(), place.Namespace, ref.App)
		if err != nil {
			problem(w, http.StatusBadGateway, "uygulamanın pod'ları okunamadı: "+err.Error())
			return
		}
		writeJSON(w, toWireMetrics(place.Namespace, ref.App, ref.Env, got))
	})
}

// inClusterMetrics builds the source from the pod's own ServiceAccount.
//
// In-cluster and never the ambient kubeconfig, for the reason inClusterLogs
// gives at length: a namespace means something only in the cluster the
// placement was written for, and following one into whatever kubectl last
// pointed at would report a stranger's pods as this tenant's.
func inClusterMetrics() (MetricsSource, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("reading pod state needs the cluster this pod is in: %w", err)
	}
	clients, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the cluster client: %w", err)
	}
	return NewClusterMetrics(clients), nil
}

// NewClusterMetrics builds a MetricsSource over a Kubernetes client, so that
// something outside this package can fill the seam.
func NewClusterMetrics(clients kubernetes.Interface) MetricsSource {
	return clusterMetrics{pods: clients.CoreV1(), raw: clients.Discovery().RESTClient()}
}

// clusterMetrics reads pods from the Kubernetes API and usage from
// metrics.k8s.io.
//
// Two sources on purpose, and they fail apart. The first is the API server
// itself and is there whenever this endpoint can run at all; the second is
// metrics-server, which an install may not have — k3s ships it and kind does
// not, and scripts/install.sh installs k3s. So the pod half is never lost to
// the absence of the usage half.
type clusterMetrics struct {
	pods corev1client.PodsGetter
	// raw carries requests to metrics.k8s.io, which has no typed client in
	// this module.
	//
	// Deliberately not adding k8s.io/metrics as a dependency for it. The whole
	// shape needed is items[].containers[].usage, that shape is stable API, and
	// a module added for one struct is a module to keep in step with client-go
	// on every upgrade.
	raw rest.Interface
}

// podMetricsList is the part of metrics.k8s.io/v1beta1 PodMetricsList this
// reads. Everything else the type carries is timestamps and window sizes, which
// only mean something to a caller sampling repeatedly.
type podMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Containers []struct {
			Name  string `json:"name"`
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

func (c clusterMetrics) ForApp(ctx context.Context, namespace, app string) (AppMetrics, error) {
	// The same selector the log endpoint uses, from the same function: two
	// endpoints that disagreed about which pods belong to an app would be a
	// user reading one app's logs beside another app's memory.
	pods, err := c.pods.Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: podSelector(app)})
	if err != nil {
		return AppMetrics{}, fmt.Errorf("pod'lar listelenemedi: %w", err)
	}

	out := AppMetrics{Pods: make([]PodSample, 0, len(pods.Items))}
	for i := range pods.Items {
		out.Pods = append(out.Pods, samplePod(&pods.Items[i]))
	}

	usage, err := c.usage(ctx, namespace, app)
	switch {
	case err != nil:
		// Not fatal. Everything above came from the API server and is true;
		// what is missing is one component, and the note says which.
		out.UsageNote = err.Error()
	default:
		out.UsageSource = usageSourceName
		matched := 0
		for i := range out.Pods {
			u, ok := usage[out.Pods[i].Name]
			if !ok {
				continue
			}
			matched++
			out.Pods[i].CPU.Usage = u.cpu
			out.Pods[i].Memory.Usage = u.memory
		}
		if matched == 0 && len(out.Pods) > 0 {
			// The API answered and had nothing to say about these pods, which
			// is not the same as the API being absent and not the same as the
			// pods using nothing. Found by running this against a crash-looping
			// app: every pod came back with no usage under a source that
			// claimed to have answered, which is the ambiguity this endpoint
			// exists to avoid.
			out.UsageNote = "metrics.k8s.io cevap verdi ama bu pod'lar için henüz örneği yok: yeni " +
				"başlamış ya da toplama aralığından hızlı yeniden başlayan bir konteyner hiç örneklenmez"
		}
	}
	return out, nil
}

// usageSourceName is what answers for CPU and memory, named once so the
// response and the note below cannot disagree about it.
const usageSourceName = "metrics.k8s.io"

// podUsage is one pod's totals, summed across its containers.
type podUsage struct{ cpu, memory *resource.Quantity }

func (c clusterMetrics) usage(ctx context.Context, namespace, app string) (map[string]podUsage, error) {
	body, err := c.raw.Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces/"+namespace+"/pods").
		Param("labelSelector", podSelector(app)).
		DoRaw(ctx)
	switch {
	case apierrors.IsNotFound(err), apierrors.IsServiceUnavailable(err):
		// The API is not registered, or it is registered and nothing is
		// serving it. Named rather than reported as a read failure: this is
		// the ordinary state of an install without metrics-server, and it is
		// not something the person reading the page did wrong.
		return nil, fmt.Errorf(
			"canlı CPU ve bellek için metrics-server gerekiyor, ve bu kümede metrics.k8s.io cevap vermiyor")
	case apierrors.IsForbidden(err):
		// Distinct from the above, because the fix is different and lives in a
		// file rather than in a helm install: the control plane's ClusterRole
		// has to grant pods on metrics.k8s.io.
		return nil, fmt.Errorf(
			"kontrol düzleminin metrics.k8s.io'yu okuma izni yok; cluster/control-plane.yaml içindeki ClusterRole'e bakın")
	case err != nil:
		return nil, fmt.Errorf("metrics.k8s.io okunamadı: %w", err)
	}

	var list podMetricsList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("metrics.k8s.io okunamayan bir cevap verdi: %w", err)
	}

	out := make(map[string]podUsage, len(list.Items))
	for _, item := range list.Items {
		// Summed across containers, because a pod with a sidecar reports two
		// and the number a user compares against the pod's limit is the total.
		cpu, memory := resource.NewQuantity(0, resource.DecimalSI), resource.NewQuantity(0, resource.BinarySI)
		for _, container := range item.Containers {
			if q, err := resource.ParseQuantity(container.Usage.CPU); err == nil {
				cpu.Add(q)
			}
			if q, err := resource.ParseQuantity(container.Usage.Memory); err == nil {
				memory.Add(q)
			}
		}
		out[item.Metadata.Name] = podUsage{cpu: cpu, memory: memory}
	}
	return out, nil
}

// samplePod reads one pod's state, and its first container's promises.
//
// The first container and not the sum, which is the one asymmetry with usage
// above and is deliberate: requests and limits are what the Workload asked for,
// a Workload renders exactly one application container, and summing in a
// sidecar the platform injected would report a limit the tenant never set.
func samplePod(pod *corev1.Pod) PodSample {
	out := PodSample{Name: pod.Name, Phase: string(pod.Status.Phase)}
	if pod.Status.StartTime != nil {
		out.StartedAt = pod.Status.StartTime.Time
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			out.Ready = cond.Status == corev1.ConditionTrue
		}
	}
	if len(pod.Spec.Containers) > 0 {
		res := pod.Spec.Containers[0].Resources
		out.CPU.Request = quantityOrNil(res.Requests, corev1.ResourceCPU)
		out.CPU.Limit = quantityOrNil(res.Limits, corev1.ResourceCPU)
		out.Memory.Request = quantityOrNil(res.Requests, corev1.ResourceMemory)
		out.Memory.Limit = quantityOrNil(res.Limits, corev1.ResourceMemory)
	}
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		out.Restarts += cs.RestartCount
		if term := cs.LastTerminationState.Terminated; term != nil && out.LastTerminated == nil {
			out.LastTerminated = &Termination{
				Reason:   term.Reason,
				ExitCode: term.ExitCode,
				At:       term.FinishedAt.Time,
			}
		}
	}
	return out
}

// quantityOrNil distinguishes "no limit set" from "a limit of zero".
//
// A pointer rather than a zero Quantity, because the two render identically and
// mean opposite things: one is an unbounded container and the other is one that
// cannot allocate. The API's own defaulting makes the second unreachable
// through this platform, which is exactly why a value that got there anyway
// must not be hidden.
func quantityOrNil(list corev1.ResourceList, name corev1.ResourceName) *resource.Quantity {
	q, ok := list[name]
	if !ok {
		return nil
	}
	return &q
}

// wireMetrics is what the panel and the CLI render.
//
// Its own type for the reason wire.go gives at length: what a sample is in this
// package is shaped by the two APIs it came from, and what a page draws is a
// product surface that will change.
//
// Quantities cross as the strings Kubernetes itself writes, with one
// normalisation that was measured rather than designed: metrics.k8s.io reports
// CPU in nanocores, so a real answer for an idle application came back as
// "518738n" where `kubectl top pod` beside it said "1m". Same number, and no
// reader would believe it. CPU is therefore rendered in milli-cores, which is
// the unit every other CPU figure in this product uses — a request of "100m",
// a limit, an HPA target. Memory is passed through as reported, because Ki and
// Mi are the same scale and rounding a small application's 7608Ki up to 8Mi
// would lose more than it tidied.
type wireMetrics struct {
	App       string `json:"app"`
	Env       string `json:"env"`
	Namespace string `json:"namespace"`

	Pods []wirePod `json:"pods"`

	// Usage says where CPU and memory came from, or why there are none.
	Usage wireUsageSource `json:"usage"`

	// Limits is what this endpoint does not answer, listed rather than left to
	// be inferred from its absence. A panel that draws a request-rate chart
	// from a field that is simply missing draws a flat line at zero.
	Limits []string `json:"limits"`
}

type wireUsageSource struct {
	Source string `json:"source,omitempty"`
	Note   string `json:"note,omitempty"`
}

type wirePod struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Ready     bool   `json:"ready"`
	Restarts  int32  `json:"restarts"`
	StartedAt string `json:"startedAt,omitempty"`

	LastTerminated *wireTermination `json:"lastTerminated,omitempty"`

	CPU    wireSample `json:"cpu"`
	Memory wireSample `json:"memory"`
}

type wireTermination struct {
	Reason   string `json:"reason"`
	ExitCode int32  `json:"exitCode"`
	At       string `json:"at,omitempty"`
}

type wireSample struct {
	Usage   string `json:"usage,omitempty"`
	Request string `json:"request,omitempty"`
	Limit   string `json:"limit,omitempty"`

	// OfLimit is usage as a percentage of the limit, when both are known.
	//
	// Computed here rather than in the panel because the alternative is every
	// surface parsing "312Mi" for itself, and a CLI that rounds differently
	// from a page is a bug report about the page.
	OfLimit *int64 `json:"ofLimitPercent,omitempty"`
}

// theseAreNotAnswered is what a caller must not read this endpoint's silence as.
//
// Written into every response rather than into a document, because the person
// who needs it is looking at a page with no latency on it and wondering whether
// their application is slow or the platform is quiet.
var theseAreNotAnswered = []string{
	"request rate and latency: the application would have to expose its own /metrics " +
		"and something would have to scrape it",
	"history: these are the values as of this request, not a series over time",
}

func toWireMetrics(namespace, app, env string, m AppMetrics) wireMetrics {
	out := wireMetrics{
		App: app, Env: env, Namespace: namespace,
		Pods:   make([]wirePod, 0, len(m.Pods)),
		Usage:  wireUsageSource{Source: m.UsageSource, Note: m.UsageNote},
		Limits: theseAreNotAnswered,
	}
	for _, p := range m.Pods {
		wp := wirePod{
			Name: p.Name, Phase: p.Phase, Ready: p.Ready, Restarts: p.Restarts,
			CPU: toWireSample(inMilli(p.CPU)), Memory: toWireSample(p.Memory),
		}
		if !p.StartedAt.IsZero() {
			wp.StartedAt = p.StartedAt.UTC().Format(time.RFC3339)
		}
		if t := p.LastTerminated; t != nil {
			wt := wireTermination{Reason: t.Reason, ExitCode: t.ExitCode}
			if !t.At.IsZero() {
				wt.At = t.At.UTC().Format(time.RFC3339)
			}
			wp.LastTerminated = &wt
		}
		out.Pods = append(out.Pods, wp)
	}
	return out
}

// inMilli rewrites a CPU sample into milli-cores.
//
// Only the usage figure needs it: requests and limits arrive from a Workload
// spec that already spells them "100m", and re-encoding those would turn a
// value the tenant typed into a value this code chose.
func inMilli(s Sample) Sample {
	if s.Usage == nil {
		return s
	}
	s.Usage = resource.NewMilliQuantity(s.Usage.MilliValue(), resource.DecimalSI)
	return s
}

func toWireSample(s Sample) wireSample {
	out := wireSample{}
	if s.Usage != nil {
		out.Usage = s.Usage.String()
	}
	if s.Request != nil {
		out.Request = s.Request.String()
	}
	if s.Limit != nil {
		out.Limit = s.Limit.String()
	}
	if s.Usage != nil && s.Limit != nil && !s.Limit.IsZero() {
		// MilliValue on both sides, so a memory limit in gibibytes and a usage
		// in mebibytes divide without either being converted to a float. A
		// limit large enough to overflow the milli scale would be 9 exabytes.
		percent := s.Usage.MilliValue() * 100 / s.Limit.MilliValue()
		out.OfLimit = &percent
	}
	return out
}
