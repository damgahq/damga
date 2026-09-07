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
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// execNS is the namespace the fixtures pretend the app lives in.
const execNS = "t-acme"

// pod builds a fixture. phase and ready are the two the API server reports
// separately and that the selection has to read separately.
func pod(name string, phase corev1.PodPhase, ready bool, containers ...string) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "t"}}
	if len(containers) == 0 {
		containers = []string{containerApp}
	}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	p.Status.Phase = phase
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}}
	return p
}

// waiting marks the pod's container as waiting with a reason, which is how a
// crash loop and an unpullable image differ from each other and from a pod
// that is merely young.
func waiting(p corev1.Pod, reason string) corev1.Pod {
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  p.Spec.Containers[0].Name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
	}}
	return p
}

// The pod a command runs in, and the refusal when there is not one.
//
// The running-and-ready case is the easy one and is not what this is for. A
// crash-looping pod, a pod that is running and failing its readiness probe,
// and a pod being deleted are all "not running" to a check that only reads the
// phase, and all three are what an app looks like when somebody reaches for a
// terminal.
func TestExecPicksARunnablePodAndSaysWhyWhenItCannot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pods  []corev1.Pod
		want  string // the pod chosen, or "" when the call must refuse
		names []string
	}{
		{
			name: "ready pod is chosen over a crash-looping one",
			pods: []corev1.Pod{
				waiting(pod("a-crash", corev1.PodPending, false), "CrashLoopBackOff"),
				pod("b-ready", corev1.PodRunning, true),
			},
			want: "b-ready",
		},
		{
			name:  "running but not ready is refused, and named as such",
			pods:  []corev1.Pod{pod("only", corev1.PodRunning, false)},
			names: []string{"only", "çalışıyor ama hazır değil"},
		},
		{
			name:  "a crash loop is named, not reported as 'not running'",
			pods:  []corev1.Pod{waiting(pod("only", corev1.PodPending, false), "CrashLoopBackOff")},
			names: []string{"CrashLoopBackOff"},
		},
		{
			name:  "an image that will not pull is a different sentence",
			pods:  []corev1.Pod{waiting(pod("only", corev1.PodPending, false), "ImagePullBackOff")},
			names: []string{"ImagePullBackOff"},
		},
		{
			name: "a terminating pod is not chosen even while it is ready",
			pods: func() []corev1.Pod {
				p := pod("going", corev1.PodRunning, true)
				now := metav1.Now()
				p.DeletionTimestamp = &now
				return []corev1.Pod{p}
			}(),
			names: []string{"going", "sonlandırılıyor"},
		},
		{
			name: "the choice does not depend on the order the API server listed them",
			pods: []corev1.Pod{
				pod("z-ready", corev1.PodRunning, true),
				pod("a-ready", corev1.PodRunning, true),
			},
			want: "a-ready",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickPod(tc.pods)
			if tc.want != "" {
				if err != nil {
					t.Fatalf("wanted pod %s, got refusal: %v", tc.want, err)
				}
				if got.Name != tc.want {
					t.Errorf("chose %s, wanted %s", got.Name, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("chose %s; none of these pods can take a command", got.Name)
			}
			if !errors.Is(err, errNoRunnablePod) {
				t.Errorf("refused with %v, which is not errNoRunnablePod", err)
			}
			for _, want := range tc.names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q, so the reader cannot tell "+
						"this apart from any other pod that is not running: %v", want, err)
				}
			}
		})
	}
}

// Which container, and the refusal to guess.
func TestExecRefusesToGuessWhichContainer(t *testing.T) {
	one := pod("p", corev1.PodRunning, true, containerApp)
	two := pod("p", corev1.PodRunning, true, containerApp, "log-shipper")

	if got, err := pickContainer(one, ""); err != nil || got != containerApp {
		t.Errorf("a pod with one container needs no argument: %q %v", got, err)
	}
	if got, err := pickContainer(two, "log-shipper"); err != nil || got != "log-shipper" {
		t.Errorf("a named container that exists must be used: %q %v", got, err)
	}

	_, err := pickContainer(two, "")
	if !errors.Is(err, errAmbiguousContainer) {
		t.Fatalf("picked a container out of two; the first one is not the app, and a "+
			"migration run in a sidecar fails as though the migration were wrong: %v", err)
	}
	for _, want := range []string{containerApp, "log-shipper", "container"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so it does not say what to pick: %v", want, err)
		}
	}

	_, err = pickContainer(two, "nope")
	if !errors.Is(err, errNoSuchContainer) {
		t.Fatalf("accepted a container the pod does not have: %v", err)
	}
	if !strings.Contains(err.Error(), "app, log-shipper") {
		t.Errorf("a wrong name should be answered with the right ones: %v", err)
	}
}

// fakeExec drives streamExec without a cluster.
type fakeExec struct {
	stdout, stderr string
	code           int
	err            error
}

func (f fakeExec) Exec(_ context.Context, _ ExecTarget, stdout, stderr io.Writer) (int, error) {
	if f.stdout != "" {
		_, _ = io.WriteString(stdout, f.stdout)
	}
	if f.stderr != "" {
		_, _ = io.WriteString(stderr, f.stderr)
	}
	return f.code, f.err
}

func runStream(t *testing.T, e Execer) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/x", nil)
	_, _ = streamExec(rec, req, e, ExecTarget{Namespace: execNS, App: appAPI, Command: []string{"true"}})
	return rec.Body.String()
}

// A command that exits non-zero ran. It is a result, not a transport failure.
//
// The distinction is the whole reason the exit code is carried separately: a
// migration that exits 1 did something and printed why, and reporting it the
// way a broken connection is reported loses both halves.
func TestExecReportsANonZeroExitAsAResult(t *testing.T) {
	body := runStream(t, fakeExec{stdout: "migrating\n", stderr: "duplicate key\n", code: 1})

	for _, want := range []string{
		`event: stdout`, `migrating`,
		`event: stderr`, `duplicate key`,
		`event: exit`, `"code":1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the stream does not carry %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "event: error") {
		t.Errorf("a command that exited 1 was reported as an error; it ran:\n%s", body)
	}
}

// A failure to run at all is an error event, and it says which failure.
func TestExecNamesWhichFailureItWas(t *testing.T) {
	target := ExecTarget{Namespace: execNS, App: appAPI}
	for _, tc := range []struct {
		name string
		err  error
		want []string
		not  string
	}{
		{
			name: "nothing deployed",
			err:  ErrNoPods,
			want: []string{appAPI, execNS, "sıfıra ölçeklendi"},
		},
		{
			name: "no pod can take it",
			err:  errNoRunnablePod,
			want: []string{"hiçbir pod çalışmıyor"},
		},
		{
			name: "the platform was never granted the right",
			err:  &apiStatusError{},
			// Names the fix and the file. The API server's own wording blames
			// the pod, which sends the reader to the wrong place entirely.
			want: []string{"pods/exec", "cluster/control-plane.yaml", "komut çalıştıramaz"},
			not:  "forbidden",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := execMessage(tc.err, target)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the message does not say %q, so it does not name this "+
						"failure: %q", want, got)
				}
			}
			if tc.not != "" && strings.Contains(strings.ToLower(got), tc.not) {
				t.Errorf("the message still reads as %q: %q", tc.not, got)
			}
		})
	}
}

// The command itself: refused when absent, bounded when absurd.
func TestExecRefusesARequestWithNoCommand(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    error
	}{
		{"no field at all", `{}`, errNoCommand},
		{"an empty list", `{"command":[]}`, errNoCommand},
		{"a blank first argument", `{"command":["   "]}`, errNoCommand},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/x", strings.NewReader(tc.body))
			_, _, err := execRequestFrom(httptest.NewRecorder(), req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("accepted %s: %v", tc.body, err)
			}
			if !strings.Contains(err.Error(), "command") {
				t.Errorf("the refusal does not say what was missing: %v", err)
			}
		})
	}

	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"command":["sh"],"container":"`+containerApp+`"}`))
	cmd, container, err := execRequestFrom(httptest.NewRecorder(), req)
	if err != nil || len(cmd) != 1 || container != containerApp {
		t.Fatalf("a well formed request was refused: %v %v %q", cmd, err, container)
	}
}

// apiStatusError is a Forbidden shaped the way apierrors.IsForbidden reads it,
// so the test exercises the real branch rather than a string match.
type apiStatusError struct{}

func (a *apiStatusError) Error() string { return `pods "api-0" is forbidden` }
func (a *apiStatusError) Status() metav1.Status {
	return metav1.Status{Reason: metav1.StatusReasonForbidden, Code: 403}
}

// execRoute with the runner injected, for driving through the real mux.
func execFake(f Execer) func(guard, stores) http.Handler {
	return func(g guard, st stores) http.Handler {
		return execWith(g, st, func() (Execer, error) { return f, nil })
	}
}

// Running a command is owner-only, and a member is told so.
//
// A member may deploy, roll back, scale and restart. None of those can read
// the environment a container was given; this can. The role table names this
// action explicitly rather than letting it fall to the default, and this is
// what holds that naming in place.
func TestExecIsOwnerOnly(t *testing.T) {
	l := newLifecycle(t)
	handler := execFake(fakeExec{stdout: "ok\n"})

	code, _ := l.callSeq(handler, "/apps/{app}/envs/{env}/exec", "", accOwner, `{"command":["true"]}`)
	if code != http.StatusOK {
		t.Errorf("an owner was refused: %d", code)
	}
	for _, who := range []string{accMember, accViewer} {
		code, body := l.callSeq(handler, "/apps/{app}/envs/{env}/exec", "", who, `{"command":["true"]}`)
		if code != http.StatusForbidden {
			t.Errorf("%s ran a command and got %d; deploying ships an image somebody "+
				"built, and this reads the environment that image was given: %s",
				who, code, body)
		}
	}
}

// The log records who ran something and what program, and never the arguments.
//
// A command line is where a password ends up — `psql -W`, a token passed as a
// flag — and this log has no access control of its own. Recording all of it
// would move a tenant's credentials somewhere their tenancy does not reach,
// which is a worse failure than a thinner audit line.
func TestExecLogsWhoRanItAndNotWhatWasInTheArguments(t *testing.T) {
	const secret = "hunter2-do-not-log-me"

	var captured bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	l := newLifecycle(t)
	code, _ := l.callSeq(execFake(fakeExec{}), "/apps/{app}/envs/{env}/exec", "", accOwner,
		`{"command":["psql","-W","`+secret+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("the command was refused (%d), so this case proves nothing about "+
			"what it would have logged", code)
	}

	logged := captured.String()
	if strings.Contains(logged, secret) {
		t.Errorf("the argument %q reached the log; a command line is where people put "+
			"passwords:\n%s", secret, logged)
	}
	for _, want := range []string{"exec started", "exec finished", accOwner, tenantHome, appAPI, "psql"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log does not carry %q, so it cannot answer who ran what:\n%s",
				want, logged)
		}
	}
}
