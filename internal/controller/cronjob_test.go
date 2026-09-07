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

package controller

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// nightly is the task every case here schedules.
const nightly = "nightly"

func withTask(task platformv1alpha1.ScheduledTask) func(*platformv1alpha1.Workload) {
	return func(a *platformv1alpha1.Workload) {
		a.Spec.Schedules = append(a.Spec.Schedules, task)
	}
}

func mustCronJob(t *testing.T, a *platformv1alpha1.Workload, i int) *batchv1.CronJob {
	t.Helper()
	job, err := desiredCronJob(a, a.Spec.Schedules[i])
	if err != nil {
		t.Fatalf("desiredCronJob: %v", err)
	}
	return job
}

// A scheduled task runs the application's own build.
//
// The reason schedules are a field of the workload rather than a resource of
// their own, asserted rather than only written down: the nightly job and the
// request handler are the same program, and a deploy that moved one without the
// other would be a version skew nobody could see from either object.
func TestAScheduledTaskRunsTheApplicationsOwnImage(t *testing.T) {
	a := app(withTask(platformv1alpha1.ScheduledTask{
		Name: nightly, Schedule: "0 3 * * *", Command: []string{"/app/report", "--daily"},
	}))

	job := mustCronJob(t, a, 0)
	container := job.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	if container.Image != a.Spec.Image {
		t.Errorf("the task runs %q and the app runs %q; a schedule that can lag the app "+
			"behind is a nightly job on last month's code", container.Image, a.Spec.Image)
	}
	if strings.Join(container.Command, " ") != "/app/report --daily" {
		t.Errorf("command = %v", container.Command)
	}
	if job.Spec.Schedule != "0 3 * * *" {
		t.Errorf("schedule = %q", job.Spec.Schedule)
	}
}

// The task and the app see the same environment.
//
// containerEnv was inline in desiredDeployment until this needed it, and the
// failure it exists to prevent has no symptom in either object alone: a
// scheduled task pointed at a different database, or missing a generated
// credential, fails at 03:00 with a connection error and everything a person
// would compare it against looks right.
func TestATaskAndItsAppSeeTheSameEnvironment(t *testing.T) {
	a := app(func(a *platformv1alpha1.Workload) {
		a.Spec.Env = []platformv1alpha1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}
		a.Spec.EnvFrom = []string{tenantSecret}
		a.Spec.UserSecrets = []string{userSecret}
		a.Spec.Database = dbName
		a.Spec.Secrets = []platformv1alpha1.GeneratedSecret{{Name: "API_KEY"}}
		a.Spec.Schedules = []platformv1alpha1.ScheduledTask{{
			Name: nightly, Schedule: "0 3 * * *", Command: []string{"/app/report"},
		}}
	})

	web := desiredDeployment(a).Spec.Template.Spec.Containers[0]
	task := mustCronJob(t, a, 0).Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	if len(task.Env) != len(web.Env) {
		t.Fatalf("the task has %d env vars and the app has %d", len(task.Env), len(web.Env))
	}
	for i := range web.Env {
		if task.Env[i] != web.Env[i] {
			t.Errorf("env[%d]: task has %v, app has %v", i, task.Env[i], web.Env[i])
		}
	}
	// Order as well as membership. The ordering is what decides which of two
	// definitions of DB_HOST wins, so a task that received the same sources in
	// a different order would resolve a variable differently from the app.
	if len(task.EnvFrom) != len(web.EnvFrom) {
		t.Fatalf("the task has %d envFrom sources and the app has %d",
			len(task.EnvFrom), len(web.EnvFrom))
	}
	for i := range web.EnvFrom {
		got, want := task.EnvFrom[i].SecretRef.Name, web.EnvFrom[i].SecretRef.Name
		if got != want {
			t.Errorf("envFrom[%d] = %q, want %q — the order decides which value wins", i, got, want)
		}
	}
}

// The hardening a Deployment gets, a CronJob gets.
//
// Written as its own case rather than folded into the Deployment's, because the
// pod spec here is built separately and "it is the same code" is exactly the
// assumption that stops being true.
func TestAScheduledTaskIsHardenedLikeEveryOtherPod(t *testing.T) {
	a := app(withTask(platformv1alpha1.ScheduledTask{
		Name: nightly, Schedule: "@daily", Command: []string{"/app/report"},
	}))
	pod := mustCronJob(t, a, 0).Spec.JobTemplate.Spec.Template.Spec
	c := pod.Containers[0]

	if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("the task pod may run as root")
	}
	if pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccomp is not RuntimeDefault")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("a service-account token is mounted into a batch pod")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("the task's root filesystem is writable")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("privilege escalation is allowed")
	}
	if c.Resources.Requests.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
		t.Error("a resource request or the memory limit is missing, which admission rejects")
	}
	// A read-only root filesystem with nowhere to write is an image that cannot
	// run. The Deployment gets an emptyDir at /tmp and so must this.
	var tmp bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/tmp" {
			tmp = true
		}
	}
	if !tmp {
		t.Error("no writable /tmp, so anything that writes a temp file fails at 03:00")
	}
}

// A batch container is not a server, and the probes that keep a server alive
// would kill it.
func TestAScheduledTaskCarriesNoProbes(t *testing.T) {
	a := app(withTask(platformv1alpha1.ScheduledTask{
		Name: nightly, Schedule: "@daily", Command: []string{"/app/migrate"},
	}))
	c := mustCronJob(t, a, 0).Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	if c.LivenessProbe != nil {
		t.Error("a liveness probe on a batch container kills a long migration at fifteen seconds")
	}
	if c.ReadinessProbe != nil || c.StartupProbe != nil {
		t.Error("a batch container answers no HTTP and is in no Service; a probe on it fails for ever")
	}
	if len(c.Ports) != 0 {
		t.Errorf("the task declares ports %v; nothing connects to it", c.Ports)
	}
}

// The four defaults this platform overrides, and what each one prevents.
//
// Every one of these differs from Kubernetes' own default, so a renderer that
// stopped setting them would still produce a working CronJob — one that
// overlaps itself, retries a failed migration six times, keeps one failed job
// out of three nights, and stops scheduling for ever after a long outage.
// Nothing about any of those reads as a misconfiguration.
func TestTheDefaultsThatDifferFromKubernetes(t *testing.T) {
	a := app(withTask(platformv1alpha1.ScheduledTask{
		Name: nightly, Schedule: "*/5 * * * *", Command: []string{"/app/batch"},
	}))
	job := mustCronJob(t, a, 0)

	if job.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Errorf("concurrencyPolicy = %q, want Forbid: Kubernetes defaults to Allow, and a run "+
			"that outlasts its interval then overlaps itself for ever", job.Spec.ConcurrencyPolicy)
	}
	if job.Spec.StartingDeadlineSeconds == nil {
		t.Error("no startingDeadlineSeconds: with none, a controller outage past a hundred " +
			"missed ticks stops this CronJob permanently and says nothing")
	}
	if job.Spec.FailedJobsHistoryLimit == nil || *job.Spec.FailedJobsHistoryLimit < 3 {
		t.Error("fewer than three failed Jobs kept; the failed ones are the ones anybody opens")
	}
	backoff := job.Spec.JobTemplate.Spec.BackoffLimit
	if backoff == nil || *backoff != 0 {
		t.Errorf("backoffLimit = %v, want 0: a scheduled task's retry policy is its schedule, "+
			"and a migration that exited non-zero must not be run five more times", backoff)
	}
	if got := job.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy; got != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never: OnFailure restarts in place and takes the "+
			"previous attempt's log with it", got)
	}
}

// Suspend is a field and not a deletion, so the schedule survives the pause.
func TestSuspendingATaskKeepsItsSchedule(t *testing.T) {
	a := app(withTask(platformv1alpha1.ScheduledTask{
		Name: nightly, Schedule: "0 3 * * *", Command: []string{"/app/report"}, Suspend: true,
	}))
	job := mustCronJob(t, a, 0)

	if job.Spec.Suspend == nil || !*job.Spec.Suspend {
		t.Error("the task is not suspended")
	}
	if job.Spec.Schedule != "0 3 * * *" {
		t.Errorf("a suspended task lost its schedule: %q", job.Spec.Schedule)
	}
}

// A name Kubernetes would refuse is refused here, with both halves.
//
// The API server's own message says a name is too long. It cannot say which two
// names were added together to make it, and the person reading it is looking at
// a Workload whose name is legal and a task whose name is legal.
func TestANameKubernetesWouldRefuseIsRefusedWithBothHalves(t *testing.T) {
	long := strings.Repeat("a", 40)
	a := app(func(a *platformv1alpha1.Workload) {
		a.Name = long
		a.Spec.Schedules = []platformv1alpha1.ScheduledTask{{
			Name: "reconcile-everything", Schedule: "@daily", Command: []string{"/app/x"},
		}}
	})

	_, err := desiredCronJob(a, a.Spec.Schedules[0])
	if err == nil {
		t.Fatal("a CronJob name over the limit was rendered; the API server would refuse it")
	}
	for _, want := range []string{long, "reconcile-everything", "52"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// And the boundary itself is usable: a name at exactly the limit renders.
	fits := app(func(a *platformv1alpha1.Workload) {
		a.Name = strings.Repeat("b", maxCronJobName-len(nightly)-1)
		a.Spec.Schedules = []platformv1alpha1.ScheduledTask{{
			Name: nightly, Schedule: "@daily", Command: []string{"/app/x"},
		}}
	})
	job, err := desiredCronJob(fits, fits.Spec.Schedules[0])
	if err != nil {
		t.Fatalf("a name of exactly %d characters was refused: %v", maxCronJobName, err)
	}
	if len(job.Name) != maxCronJobName {
		t.Errorf("the boundary case is %d characters, so it did not test the boundary", len(job.Name))
	}
}

// Every rendered CronJob is findable as one of this workload's.
//
// The label is what the prune pass searches on, so a renderer that stopped
// setting it would leave a schedule running after it was deleted from the
// manifest — and the prune would report success having found nothing.
func TestEveryTaskIsLabelledWithItsName(t *testing.T) {
	a := app(withTask(platformv1alpha1.ScheduledTask{
		Name: nightly, Schedule: "@daily", Command: []string{"/app/report"},
	}))
	job := mustCronJob(t, a, 0)

	if got := job.Labels[scheduledTaskLabel]; got != nightly {
		t.Errorf("%s = %q, want %q; the prune pass finds nothing without it",
			scheduledTaskLabel, got, nightly)
	}
	for k, v := range selectorFor(a) {
		if job.Labels[k] != v {
			t.Errorf("label %s = %q, want %q; the prune pass lists by this selector",
				k, job.Labels[k], v)
		}
	}
}
