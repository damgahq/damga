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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/placement"
)

const (
	// defaultTail is how much history a page gets before the stream becomes
	// live. Enough that a crash loop's last words are on screen without
	// scrolling; small enough that opening the page is not a query for a
	// megabyte per container.
	defaultTail = 200

	// maxTail is what a caller may ask for. A ceiling rather than a limit on
	// the response, because the cost is paid by the API server and the kubelet
	// before a single byte reaches this process.
	maxTail = 5000

	// logHeartbeat keeps the connection observably alive.
	//
	// An idle application writes nothing, and "nothing arrived" is what a dead
	// connection looks like too — to the reader and to every proxy between
	// them. ingress-nginx closes a stream that has been silent for 60 seconds
	// by default, so the beat has to be well inside that.
	logHeartbeat = 20 * time.Second

	// logWriteTimeout bounds one write to a reader that has stopped reading.
	// Without it a browser tab left open on a suspended laptop holds this
	// goroutine, and every pod stream feeding it, until the process restarts.
	logWriteTimeout = 30 * time.Second

	// maxLogLine is where a line is cut. A container that writes a 40 MB JSON
	// blob on one line is a container that would otherwise decide this
	// process's memory.
	maxLogLine = 64 << 10
)

// ErrNoPods is a selector that matched nothing.
//
// Its own error because it is not a failure: an app that is scaled to zero, or
// has never been deployed, has no logs and that is the true answer. Reported
// separately so the page can say which of the two silences this is.
var ErrNoPods = errors.New("server: no pods are running for this app")

// LogLine is one line, from one container.
type LogLine struct {
	Pod       string
	Container string
	// At is when the container wrote it, as the cluster recorded it. Zero when
	// the line carried no timestamp, which is a line and not an error.
	At   time.Time
	Text string
}

// LogSelector is which lines.
type LogSelector struct {
	// Namespace comes from the placement row and never from a convention, for
	// the reason placement carries it as a field: a namespace derived from a
	// tenant and an environment is a name parsed out of an identity.
	Namespace string
	App       string
	Tail      int64
	Follow    bool
}

// LogSource is where lines come from.
//
// A seam for the same reason BackupReader is one: this is a second place the
// control plane reads the cluster, and an install with no cluster to read must
// still start, serve and say so. It is deliberately not a field on Options —
// see logs for what fills it and why that is not configuration.
//
// emit is never called concurrently. An implementation that reads several
// containers at once serializes them itself, so that nothing downstream has to
// own a lock to write a line.
type LogSource interface {
	Stream(ctx context.Context, sel LogSelector, emit func(LogLine) error) error
}

// logs streams what an app's containers are writing, as they write it.
//
// # Why this GET carries a CSRF check of its own
//
// run.go wraps everything in http.CrossOriginProtection, and that control
// always allows GET, HEAD and OPTIONS: a safe method cannot change state, so
// forging one achieves nothing. This endpoint is the exception the comment
// there predicted. It is a GET, and what it hands back is a tenant's own
// output — so a page on another origin that can make the browser open it, with
// the session cookie attached, reads that output. The exemption is exactly
// wrong here and is not inherited.
//
// # Why the source is not an Options field
//
// Builds and Backups are both filled by the composition root, and this one is
// not: it is resolved from the pod's own ServiceAccount at first use, and from
// nothing else. See inClusterLogs.
func logs(g guard, st stores) http.Handler {
	// Resolved once and reused. Building a client parses a token and a CA
	// bundle off disk, and a panel whose stream reconnects — which is what
	// EventSource does for a living — would otherwise pay for that on every
	// reconnect. A failure is cached with the same finality, and that is
	// correct rather than convenient: what it reads is an environment variable
	// and a mounted file, and neither appears later in a process that started
	// without them.
	return logStream(g, st, sync.OnceValues(inClusterLogs))
}

// logStream is logs with the source injected, which is how a test drives it
// without a cluster.
func logStream(g guard, st stores, open func() (LogSource, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := sameOrigin(r); err != nil {
			// Before the session is even resolved. What is being refused is the
			// browser that was made to ask, not the person whose cookie it
			// carried, and answering "not signed in" to a cross-origin probe
			// tells it whether the cookie was any good.
			problem(w, http.StatusForbidden, "çapraz kaynaklı istek reddedildi")
			return
		}
		_, ref, ok := g.admit(w, r, authz.ActionAppView)
		if !ok {
			return
		}

		sel, err := logSelectorFrom(r)
		if err != nil {
			problem(w, http.StatusBadRequest, err.Error())
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
		sel.Namespace, sel.App = place.Namespace, ref.App

		src, err := open()
		if err != nil {
			// The same shape createBuild uses, and for the same reason: a
			// control plane that is not in a cluster cannot read a pod's
			// output, and reporting that as an empty stream would be
			// indistinguishable from an application that is saying nothing.
			slog.Warn("log streaming is unavailable", "error", err)
			problem(w, http.StatusNotImplemented,
				"bu kurulum kümeden log okuyamıyor")
			return
		}

		streamSSE(w, r, src, sel)
	})
}

// streamSSE writes the event stream until the source finishes or the client
// leaves.
//
// Everything below the first byte is an event and never a status code. Once
// 200 and the headers are on the wire the status is spent, and a stream that
// answers a mid-flight failure by simply stopping is one the reader cannot
// tell from an application that went quiet.
func streamSSE(w http.ResponseWriter, r *http.Request, src LogSource, sel LogSelector) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	// no-transform as well as no-cache: a proxy that gzips this buffers it,
	// and a buffered stream is a stream that arrives in one piece at the end.
	h.Set("Cache-Control", "no-cache, no-transform")
	// nginx buffers proxied responses by default, including this one, and this
	// header is the documented way to ask it not to. The platform's own ingress
	// is ingress-nginx, so the reader would otherwise see nothing until the
	// buffer filled.
	//
	// Connection: keep-alive is deliberately absent. It is meaningless on
	// HTTP/1.1, where it is the default, and forbidden on HTTP/2.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// Flushed before anything is sent, so the reader's onopen fires now rather
	// than at the first line. An app that writes once an hour would otherwise
	// look like a stream that never opened.
	_ = rc.Flush()

	ctx := r.Context()
	// The writes all happen on this goroutine; the source's do not. A buffer
	// so a burst does not block the container that produced it, and a size
	// rather than none so a reader that has stalled is bounded.
	lines := make(chan LogLine, 256)
	done := make(chan error, 1)
	go func() {
		done <- src.Stream(ctx, sel, func(l LogLine) error {
			select {
			case lines <- l:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	beat := time.NewTicker(logHeartbeat)
	defer beat.Stop()

	var sent int
	write := func(event string, payload any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(logWriteTimeout))
		if err := writeSSE(w, event, payload); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	for {
		select {
		case l := <-lines:
			if !write("line", asLogEvent(l)) {
				return
			}
			sent++
		case <-beat.C:
			_ = rc.SetWriteDeadline(time.Now().Add(logWriteTimeout))
			// A comment frame. EventSource discards it, and every hop in
			// between learns the connection is alive.
			if _, err := fmt.Fprint(w, ": beat\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		case err := <-done:
			// Whatever the source handed over before it returned. Dropping it
			// would lose the tail of every finite stream — the last lines
			// before a container exited are the ones being read for.
			for {
				select {
				case l := <-lines:
					if !write("line", asLogEvent(l)) {
						return
					}
					sent++
					continue
				default:
				}
				break
			}
			switch {
			case err == nil:
				write("end", map[string]any{"lines": sent})
			case errors.Is(err, ErrNoPods):
				write("end", map[string]any{"lines": sent, "reason": "bu uygulama için çalışan bir şey yok"})
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				// The reader left. Nothing to report to them about it.
			default:
				slog.Warn("log stream failed", "namespace", sel.Namespace, "app", sel.App, "error", err)
				// The message is this platform's own and never the cluster's:
				// an API server refusal quotes the ServiceAccount and the
				// resource, and that belongs in the log this line just wrote.
				write("error", map[string]any{"detail": "log akışı durdu"})
			}
			return
		case <-ctx.Done():
			return
		}
	}
}

// logEvent is one line as the page receives it.
type logEvent struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// At is RFC3339 with nanoseconds, or empty. A string rather than a number
	// because the panel formats it with the same helper every other timestamp
	// on the page goes through.
	At   string `json:"at"`
	Text string `json:"text"`
}

func asLogEvent(l LogLine) logEvent {
	e := logEvent{Pod: l.Pod, Container: l.Container, Text: l.Text}
	if !l.At.IsZero() {
		e.At = l.At.UTC().Format(time.RFC3339Nano)
	}
	return e
}

// writeSSE writes one event.
//
// The payload is JSON, and that is what makes a single data line safe: a log
// line is arbitrary bytes and a raw newline inside one would end the event
// early, silently, splitting one message into two. json.Marshal escapes it.
func writeSSE(w http.ResponseWriter, event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	return err
}

// crossOrigin is the standard library's CSRF rule, held here so this endpoint
// can ask it a question the middleware in run.go does not.
var crossOrigin = http.NewCrossOriginProtection()

// sameOrigin refuses a cross-origin browser request, including a GET.
//
// Asked rather than re-implemented. The rule wanted here is precisely the one
// run.go installs, minus the clause that exempts safe methods — and the
// alternative to borrowing it is a second copy of "read Sec-Fetch-Site, then
// compare Origin against Host" living in this repository, free to drift from
// the standard library's copy on the day a browser sends something neither
// anticipated.
//
// The request is copied and its method changed to one the rule does not
// exempt. Check reads the method, the headers and the host and mutates
// nothing, so a shallow copy is enough and the original is untouched.
func sameOrigin(r *http.Request) error {
	probe := *r
	probe.Method = http.MethodPost
	return crossOrigin.Check(&probe)
}

// logSelectorFrom reads the query. It carries no namespace: that comes from the
// placement row, and a caller who could name it could read another tenant's
// pods with their own valid session.
func logSelectorFrom(r *http.Request) (LogSelector, error) {
	sel := LogSelector{Tail: defaultTail, Follow: true}

	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		// Refused rather than defaulted. A page that asked for 500 lines and
		// silently received 200 shows a gap where the missing lines were, and
		// nothing on screen says the request was ignored.
		if err != nil || n < 0 {
			return LogSelector{}, fmt.Errorf("tail bir satır sayısı olmalı, %q değil", raw)
		}
		if n > maxTail {
			return LogSelector{}, fmt.Errorf("tail en fazla %d satır olabilir", maxTail)
		}
		sel.Tail = n
	}

	// follow=false is the only thing that turns it off, so a typo asks for a
	// live stream rather than quietly getting a snapshot.
	if raw := r.URL.Query().Get("follow"); raw != "" {
		follow, err := strconv.ParseBool(raw)
		if err != nil {
			return LogSelector{}, fmt.Errorf("follow true ya da false olmalı, %q değil", raw)
		}
		sel.Follow = follow
	}
	return sel, nil
}

// podSelector is the label pair internal/controller/resources.go puts on every
// pod it renders, spelled here because that function is unexported and this
// package cannot import the controller.
//
// A duplication worth naming, the same way BuildNamespace's is: the operator
// writes these labels and this reads them, they have to agree, and nothing
// checks that they do.
//
// Concatenated rather than escaped, and that is safe for a reason rather than
// by luck. The app name is validated as a Kubernetes name before a placement
// row exists (see createApp), and a label selector has no OR: every term a
// caller could smuggle in can only narrow the match. The namespace — which is
// the actual boundary — is not in the query at all.
func podSelector(app string) string {
	return "app.kubernetes.io/name=" + app + ",app.kubernetes.io/instance=" + app
}

// splitStamp separates the timestamp the kubelet prefixes from the line the
// container wrote.
//
// A line that does not start with one is returned whole. That is not a
// tolerance: the prefix is added by the API server when it is asked for, and a
// container is perfectly entitled to write something that looks like a date.
func splitStamp(line string) (time.Time, string) {
	stamp, text, ok := strings.Cut(line, " ")
	if !ok {
		return time.Time{}, line
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, line
	}
	return at, text
}

// inClusterLogs builds the source from the pod's own ServiceAccount.
//
// In-cluster and never the ambient kubeconfig, which is the one place this
// differs from clusterReader. A namespace is meaningful only in the cluster the
// placement row was written for; a developer running this binary on a laptop
// has a kubeconfig pointing wherever kubectl last pointed, and following a
// tenant's namespace into that cluster would stream whatever happens to be
// there under that name. Out of a cluster, this endpoint answers 501.
//
// # This cannot work yet, and the reason is one file away
//
// cluster/control-plane.yaml grants the control plane get, list and watch on
// workloads, databases and deployments. Reading a container's output needs pods
// and pods/log, and neither is in that ClusterRole — so this returns a source
// that the API server will refuse. That grant is a decision about what the
// control plane may read, and it is made in that file rather than assumed here.
func inClusterLogs() (LogSource, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("reading logs needs the cluster this pod is in: %w", err)
	}
	clients, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the cluster client: %w", err)
	}
	return clusterLogs{pods: clients.CoreV1()}, nil
}

// clusterLogs reads pod output through the Kubernetes API.
type clusterLogs struct{ pods corev1client.PodsGetter }

// NewClusterLogs builds a LogSource over a Kubernetes client, so that something
// outside this package can fill the seam.
func NewClusterLogs(clients kubernetes.Interface) LogSource {
	return clusterLogs{pods: clients.CoreV1()}
}

// Stream follows every container of every pod the app has.
//
// Every container, not the first: a workload with a sidecar writes the
// interesting half of its output from the container nobody would have picked.
func (c clusterLogs) Stream(ctx context.Context, sel LogSelector, emit func(LogLine) error) error {
	pods, err := c.pods.Pods(sel.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: podSelector(sel.App),
	})
	if err != nil {
		return fmt.Errorf("listing the pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return ErrNoPods
	}

	// One lock around emit, so that the contract LogSource states is true: the
	// reader never sees two lines being written at once and needs no lock of
	// its own.
	var mu sync.Mutex
	var wg sync.WaitGroup
	var opened, failed int64
	var first error
	tail := sel.Tail

	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			wg.Go(func() {
				err := c.follow(ctx, sel, pod.Name, container.Name, tail, &mu, emit)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failed++
					if first == nil {
						first = err
					}
					return
				}
				opened++
			})
		}
	}
	wg.Wait()

	// A container that has just been replaced answers "container not found",
	// and a pod that is starting answers "is waiting to start". Neither is
	// worth failing a stream that is otherwise delivering, so the error is
	// reported only when nothing worked at all — which is what a missing
	// pods/log grant looks like.
	if opened == 0 && failed > 0 {
		return first
	}
	return nil
}

func (c clusterLogs) follow(
	ctx context.Context, sel LogSelector, pod, container string, tail int64,
	mu *sync.Mutex, emit func(LogLine) error,
) error {
	opts := &corev1.PodLogOptions{
		Container: container,
		Follow:    sel.Follow,
		// Asked for, and the reason splitStamp exists: without it a line
		// carries no time at all, and "when did it say that" is most of what a
		// log is read for.
		Timestamps: true,
	}
	if tail > 0 {
		opts.TailLines = &tail
	}
	body, err := c.pods.Pods(sel.Namespace).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		return fmt.Errorf("opening %s/%s: %w", pod, container, err)
	}
	defer func() { _ = body.Close() }()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 4096), maxLogLine)
	for scanner.Scan() {
		at, text := splitStamp(scanner.Text())
		mu.Lock()
		err := emit(LogLine{Pod: pod, Container: container, At: at, Text: text})
		mu.Unlock()
		if err != nil {
			return err
		}
	}
	// A line longer than the buffer ends the scan with an error, and the lines
	// already delivered are still delivered. Reported rather than swallowed,
	// because "the stream stopped" and "one container writes 64 KB lines" are
	// different problems with the same symptom.
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading %s/%s: %w", pod, container, err)
	}
	return nil
}
