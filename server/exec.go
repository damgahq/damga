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
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/placement"
)

const (
	// maxCommandArgs and maxCommandArg bound what a request may ask to run.
	// Not a security control — anybody who reaches this endpoint may already
	// run a shell — but a bound on what this process will hold in memory
	// before it has decided anything.
	maxCommandArgs = 64
	maxCommandArg  = 4 << 10

	// execHeartbeat is why this streams rather than answering in one piece.
	//
	// The named case for this endpoint is "run a migration", which is the case
	// that takes minutes and prints nothing while it does. A buffered response
	// to a request that silent is closed by ingress-nginx after 60 seconds
	// (see logHeartbeat, which exists for the same reason) — and the command
	// has already run by then, so the caller loses the result of something
	// that did happen. That is the one outcome worth engineering against here.
	execHeartbeat = 20 * time.Second
)

// Errors this endpoint tells apart.
//
// One error for "exec failed" would be true of all of them at once, and each
// of these has a different next move: deploy the app, wait, say which
// container, fix the name, send a command, grant a permission. A message that
// cannot say which one is a message the reader has to guess from.
var (
	// errNoCommand: the request body carried no argv.
	errNoCommand = errors.New("hiçbir komut verilmedi: en az bir eleman içeren {\"command\": [...]} gönderin")

	// errNoRunnablePod: pods exist and none of them can take a command. The
	// message names the pods and what each is doing, because "not running" is
	// where a crash loop, an image that will not pull and a pod still being
	// created all look identical.
	errNoRunnablePod = errors.New("hiçbir pod çalışmıyor")

	// errAmbiguousContainer: the pod has several containers and the request
	// named none. Refused rather than guessed — picking the first would run a
	// migration in a sidecar on the day somebody adds one.
	errAmbiguousContainer = errors.New("bu pod'da birden fazla konteyner var")

	// errNoSuchContainer: the named container is not in the pod.
	errNoSuchContainer = errors.New("böyle bir konteyner yok")
)

// ExecTarget is one command, in one container, of one pod.
type ExecTarget struct {
	// Namespace comes from the placement row, never from a convention, for the
	// reason placement carries it as a field.
	Namespace string
	App       string
	// Container may be empty, and the selection below decides what that means:
	// the only container, or a refusal naming the choices.
	Container string
	Command   []string
}

// Execer runs a command in a container and streams what it wrote.
//
// A seam for the reason LogSource is one: this is another place the control
// plane reaches into the cluster, and an install with no cluster to reach must
// still start, serve, and say so rather than appearing broken.
//
// Deliberately not a field on Options, and that is the difference between this
// and Builds. Builds is filled by the composition root because an installation
// may legitimately answer builds a different way; this is resolved from the
// pod's own ServiceAccount at first use and from nothing else. See
// inClusterExec. Nothing that constructs Options gains a field, so no existing
// call site quietly means something new by leaving one empty.
type Execer interface {
	// Exec returns the command's exit code. A non-zero code is a result and
	// not an error: a migration that exits 1 ran, and the caller needs to be
	// told what it printed and what it returned.
	Exec(ctx context.Context, t ExecTarget, stdout, stderr io.Writer) (int, error)
}

// execRoute runs one command in a tenant's container.
//
// # Why one command and not a terminal
//
// A browser terminal needs a terminal emulator, and the panel has no build
// step and no npm — panel.go states that as a decision and says it will not
// grow one. Shipping xterm.js would cost this repository the property that
// its front end has one dependency tree, in exchange for interactivity that
// the case this was asked for does not need: "run a migration" is one command
// whose output you read.
//
// # Why it streams
//
// See execHeartbeat. The command runs whether or not the connection survives,
// so the design question is whether the caller finds out what it did.
//
// # Why this is not an evidence record
//
// It was the first thing tried, and it is wrong for a reason worth writing
// down rather than rediscovering. An evidence Record is a deploy: its Seq
// means "the 41st deploy of api/prod", its Source is a commit, its Image is
// what that commit shipped, and its Hash chains to the record before it so
// that Verify can walk one Ref and say the history is intact. An exec has
// none of those. It carries no commit and no image, it has no lifecycle to
// transition through, and nothing about it can be replayed or verified later
// — the container it touched is gone by the next sync.
//
// Appending one anyway would do two kinds of damage. It would make Seq stop
// counting deploys, so "the 41st deploy" becomes a number that includes the
// times somebody opened a shell. And it would put a link in the hash chain
// that Verify has to accept but cannot check, which is worse than not
// recording it: a chain whose links mean two different things is a chain that
// proves less than it appears to.
//
// So it is a log line — see the two slog calls in execWith — and that is a
// weaker record, honestly weaker. It is not queryable per tenant, it is not
// tamper-evident, and it is not exported with the rest. What it is not is a
// deploy record that lies about what it counts. A tenant-visible audit trail
// for actions that are not deploys is a second store and a real piece of
// work; this endpoint is the second caller that wants one (the first is
// member:invite), and when there is a third it should be built rather than
// borrowed from the chain.
func execRoute(g guard, st stores) http.Handler {
	// Resolved once and reused, for the reason logs does the same: building a
	// client parses a token and a CA bundle off disk.
	return execWith(g, st, sync.OnceValues(inClusterExec))
}

// execWith is execRoute with the runner injected, which is how a test drives
// it without a cluster.
func execWith(g guard, st stores, open func() (Execer, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ref, ok := g.admit(w, r, authz.ActionAppExec)
		if !ok {
			return
		}

		cmd, container, err := execRequestFrom(w, r)
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

		runner, err := open()
		if err != nil {
			// The shape createBuild uses, and for the same reason: a control
			// plane outside a cluster cannot exec into a pod, and saying so is
			// different from saying the command failed.
			slog.Warn("exec is unavailable", "error", err)
			problem(w, http.StatusNotImplemented,
				"bu kurulum kümede komut çalıştıramıyor")
			return
		}

		// Recorded before it runs, and again with what it returned. Which of
		// those two lines exists tells a reader whether a command that is not
		// accounted for was refused or was cut off mid-flight.
		//
		// argv[0] and the argument count, never the whole command line. A
		// command line is where people put a password — `psql -W hunter2`, a
		// token in an environment assignment — and this log has no access
		// control of its own, so recording all of it would move a tenant's
		// credentials somewhere their tenancy does not reach. What is kept is
		// enough to answer who ran something and roughly what: the identity,
		// the app, the container and the program.
		//
		// See the note on evidence in this file for why it is a log line and
		// not a record in the chain.
		slog.Info("exec started",
			"actor", sub.ID, "email", sub.Email, "tenant", ref.TenantID,
			"app", ref.App, "env", ref.Env, "namespace", place.Namespace,
			"container", container, "program", cmd[0], "args", len(cmd)-1)

		code, err := streamExec(w, r, runner, ExecTarget{
			Namespace: place.Namespace, App: ref.App,
			Container: container, Command: cmd,
		})
		slog.Info("exec finished",
			"actor", sub.ID, "tenant", ref.TenantID, "app", ref.App, "env", ref.Env,
			"program", cmd[0], "code", code, "error", err)
	})
}

// execRequestFrom reads the command and the optional container from the body.
func execRequestFrom(w http.ResponseWriter, r *http.Request) ([]string, string, error) {
	var body struct {
		Command   []string `json:"command"`
		Container string   `json:"container"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return nil, "", fmt.Errorf("istek okunamadı: %w", err)
	}
	if len(body.Command) == 0 {
		return nil, "", errNoCommand
	}
	if len(body.Command) > maxCommandArgs {
		return nil, "", fmt.Errorf("komutta %d argüman var, sınır ise %d",
			len(body.Command), maxCommandArgs)
	}
	for i, arg := range body.Command {
		if len(arg) > maxCommandArg {
			return nil, "", fmt.Errorf("%d numaralı argüman %d bayt, sınır ise %d",
				i, len(arg), maxCommandArg)
		}
	}
	// An empty first element is a command that resolves to nothing, and the
	// kubelet's answer to it names neither the argument nor the request.
	if strings.TrimSpace(body.Command[0]) == "" {
		return nil, "", errNoCommand
	}
	return body.Command, body.Container, nil
}

// streamExec runs the command and writes what it produced as an event stream.
//
// Everything after the first byte is an event and never a status code, for the
// reason streamSSE says: once the headers are on the wire the status is spent,
// and a stream that reports a failure by stopping is one the reader cannot
// tell from a command that printed nothing.
func streamExec(w http.ResponseWriter, r *http.Request, runner Execer, t ExecTarget) (int, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// The stream is a tenant's own output; no proxy on the way should hold a
	// copy of it.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	out := &sseWriter{w: w, flusher: flusher}

	// The heartbeat and the command's own output share one writer, so the
	// ticker cannot interleave a beat into the middle of a chunk.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var beat sync.WaitGroup
	beat.Go(func() {
		tick := time.NewTicker(execHeartbeat)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				out.beat()
			}
		}
	})

	code, err := runner.Exec(ctx, t, out.stream("stdout"), out.stream("stderr"))
	cancel()
	beat.Wait()

	if err != nil {
		out.event("error", map[string]string{"message": execMessage(err, t)})
		return code, err
	}
	out.event("exit", map[string]any{"code": code})
	return code, nil
}

// execMessage turns a failure into a sentence that names which failure it was.
//
// The API server's own answer to a missing permission is "pods \"x\" is
// forbidden", which reads as the pod refusing rather than as this installation
// never having been granted the right. That distinction is the whole reason
// this function exists: one of these is fixed by an operator editing RBAC and
// the rest are not.
func execMessage(err error, t ExecTarget) string {
	switch {
	case errors.Is(err, ErrNoPods):
		return fmt.Sprintf("%s uygulamasının %s içinde hiç pod'u yok: ya hiçbir şey deploy edilmedi ya da sıfıra ölçeklendi",
			t.App, t.Namespace)
	case errors.Is(err, errNoRunnablePod), errors.Is(err, errAmbiguousContainer),
		errors.Is(err, errNoSuchContainer):
		// These already name themselves and what they saw.
		return err.Error()
	case apierrors.IsForbidden(err):
		return "bu kontrol düzlemi pod'larda komut çalıştıramaz. ClusterRole'ü " +
			"pods ve pods/log veriyor, ve ona pods/exec eklenmesi gerekiyor — " +
			"bkz. cluster/control-plane.yaml"
	case apierrors.IsNotFound(err):
		return "pod seçildiğinde oradaydı, şimdi orada değil; muhtemelen komut " +
			"sürerken değiştirildi"
	default:
		return err.Error()
	}
}

// sseWriter serializes everything written to one event stream.
//
// stdout and stderr are two io.Writers over one connection, and remotecommand
// writes to both from different goroutines. Without the lock a chunk of stderr
// lands inside a stdout event and the reader sees neither.
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s *sseWriter) event(name string, payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = writeSSE(s.w, name, payload)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// beat keeps the connection observably alive through a command that is working
// and saying nothing. A comment rather than an event, so a reader counting
// output does not count it.
func (s *sseWriter) beat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprint(s.w, ": beat\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *sseWriter) stream(name string) io.Writer {
	return streamOf{parent: s, name: name}
}

type streamOf struct {
	parent *sseWriter
	name   string
}

func (o streamOf) Write(p []byte) (int, error) {
	// Reported as written whatever the connection did. The command is running
	// in the cluster and cannot be un-run by a reader that left, and returning
	// an error here would abort it half way — which is the worst of both.
	o.parent.event(o.name, map[string]string{"text": string(p)})
	return len(p), nil
}

func inClusterExec() (Execer, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("running a command needs the cluster this pod is in: %w", err)
	}
	clients, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the cluster client: %w", err)
	}
	return clusterExec{clients: clients, cfg: cfg}, nil
}

// clusterExec runs commands through the Kubernetes API.
type clusterExec struct {
	clients kubernetes.Interface
	cfg     *rest.Config
}

// NewClusterExec builds an Execer over a Kubernetes client, so something
// outside this package can fill the seam.
func NewClusterExec(clients kubernetes.Interface, cfg *rest.Config) Execer {
	return clusterExec{clients: clients, cfg: cfg}
}

func (c clusterExec) Exec(ctx context.Context, t ExecTarget, stdout, stderr io.Writer) (int, error) {
	pods, err := c.clients.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: podSelector(t.App),
	})
	if err != nil {
		return 0, fmt.Errorf("pod'lar listelenemedi: %w", err)
	}
	if len(pods.Items) == 0 {
		return 0, ErrNoPods
	}
	pod, err := pickPod(pods.Items)
	if err != nil {
		return 0, err
	}
	container, err := pickContainer(pod, t.Container)
	if err != nil {
		return 0, err
	}

	req := c.clients.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   t.Command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.cfg, http.MethodPost, req.URL())
	if err != nil {
		return 0, fmt.Errorf("komut hazırlanamadı: %w", err)
	}
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdout, Stderr: stderr,
	})
	// A non-zero exit is the command's answer, not a transport failure. It
	// arrives here as an error and has to stop being one, or every migration
	// that legitimately exits 1 reads as a broken platform.
	var coded utilexec.CodeExitError
	if errors.As(err, &coded) {
		return coded.Code, nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// pickPod chooses the pod a command will run in.
//
// Running AND Ready, not merely Running. A pod that has started its container
// but is failing its readiness probe is one the platform is deliberately
// keeping traffic away from, and it is usually mid-crash-loop — running a
// migration in it is running it in the pod most likely to be killed while the
// command is in flight.
//
// When nothing qualifies, the refusal names every pod and what it is doing.
// "No pod is running" on its own is the message that sent somebody to look at
// a Deployment when the answer was an image that would not pull.
func pickPod(pods []corev1.Pod) (corev1.Pod, error) {
	sorted := make([]corev1.Pod, len(pods))
	copy(sorted, pods)
	// By name, so that two identical clusters pick the same pod and a caller
	// running the same command twice does not get two different containers for
	// reasons the API server's list order decides.
	slices.SortFunc(sorted, func(a, b corev1.Pod) int {
		return strings.Compare(a.Name, b.Name)
	})

	for _, pod := range sorted {
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil && isReady(pod) {
			return pod, nil
		}
	}

	states := make([]string, 0, len(sorted))
	for _, pod := range sorted {
		states = append(states, pod.Name+" ("+podState(pod)+")")
	}
	return corev1.Pod{}, fmt.Errorf("%w: %s", errNoRunnablePod, strings.Join(states, ", "))
}

func isReady(pod corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podState is the shortest true sentence about why a pod cannot take a command.
//
// The container's waiting reason first, because it is the specific one:
// CrashLoopBackOff, ImagePullBackOff and ContainerCreating all present as a
// pod that is not Running, and they are three different problems with three
// different fixes.
func podState(pod corev1.Pod) string {
	if pod.DeletionTimestamp != nil {
		return "sonlandırılıyor"
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return w.Reason
		}
	}
	if pod.Status.Phase == corev1.PodRunning && !isReady(pod) {
		return "çalışıyor ama hazır değil"
	}
	return string(pod.Status.Phase)
}

// pickContainer chooses which container in the pod, or refuses to.
//
// A pod with one container needs no argument and a pod with several gets no
// guess. The first container is not the app: a workload with a sidecar puts
// the interesting one wherever the template happens to list it, and a
// migration run in a log shipper fails in a way that looks like the migration
// being wrong.
func pickContainer(pod corev1.Pod, want string) (string, error) {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	if want != "" {
		if slices.Contains(names, want) {
			return want, nil
		}
		return "", fmt.Errorf("%w: %s pod'unda %q; içinde %s var",
			errNoSuchContainer, pod.Name, want, strings.Join(names, ", "))
	}
	switch len(names) {
	case 0:
		// Not reachable through the API server, which rejects a pod with no
		// containers. Answered anyway, because the alternative is indexing
		// names[0] on the day something else builds a Pod value.
		return "", fmt.Errorf("%s pod'u hiçbir konteyner bildirmiyor", pod.Name)
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("%w (%s); hangisi olduğunu {\"container\": \"...\"} ile söyleyin",
			errAmbiguousContainer, strings.Join(names, ", "))
	}
}
