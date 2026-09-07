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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

const (
	s3Bucket = "archives"
	s3Secret = "acme-s3"

	// The value that must never turn up anywhere but a SecretKeyRef.
	s3TestSecretValue = "aB/cD+eF=gH1234567890123456789012"
)

func backedUp(s3 *platformv1alpha1.S3Destination) *platformv1alpha1.Database {
	db := &platformv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "tenant-a"},
		Spec: platformv1alpha1.DatabaseSpec{
			Image:   testPostgresImage,
			Storage: resource.MustParse("1Gi"),
			Backup: &platformv1alpha1.DatabaseBackup{
				Schedule: "0 2 * * *", RetainDays: 7,
				Storage: resource.MustParse("1Gi"),
				S3:      s3,
			},
		},
	}
	normaliseDatabase(db)
	return db
}

func destination() *platformv1alpha1.S3Destination {
	return &platformv1alpha1.S3Destination{
		Bucket: s3Bucket, Endpoint: "https://minio.example.test:9000",
		Prefix: "shop", CredentialsSecret: s3Secret,
	}
}

// The path that was already proven stays exactly as it was. A Database with no
// destination has to render the pod it always rendered: one container, no init
// containers, and nothing pulled that it does not use.
func TestWithoutADestinationTheBackupPodIsUnchanged(t *testing.T) {
	pod := desiredBackupCronJob(backedUp(nil)).Spec.JobTemplate.Spec.Template.Spec

	if len(pod.InitContainers) != 0 {
		t.Errorf("a local-only backup grew %d init containers", len(pod.InitContainers))
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Image != testPostgresImage {
		t.Fatalf("a local-only backup runs %d container(s); it should be the database's own "+
			"image and nothing else", len(pod.Containers))
	}
	if got := pod.Containers[0].Command[2]; got != backupScript {
		t.Error("the single-container path stopped running the script it has always run")
	}
}

// Three containers, in the order dump, ship, restore. No image can do all of
// it: pg_dump and initdb are in the database's image and an S3 client is not.
func TestADestinationSplitsTheWorkAcrossThreeContainers(t *testing.T) {
	pod := desiredBackupCronJob(backedUp(destination())).Spec.JobTemplate.Spec.Template.Spec

	if len(pod.InitContainers) != 2 || len(pod.Containers) != 1 {
		t.Fatalf("got %d init and %d containers, want 2 and 1",
			len(pod.InitContainers), len(pod.Containers))
	}
	want := []struct{ name, image string }{
		{backupContainer, testPostgresImage},
		{shipContainer, mcImage},
	}
	for i, w := range want {
		if pod.InitContainers[i].Name != w.name || pod.InitContainers[i].Image != w.image {
			t.Errorf("init container %d is %s/%s, want %s/%s; the order is dump then ship, "+
				"because the archive has to exist before it can be copied",
				i, pod.InitContainers[i].Name, pod.InitContainers[i].Image, w.name, w.image)
		}
	}
	if pod.Containers[0].Name != rehearseContainer || pod.Containers[0].Image != testPostgresImage {
		t.Errorf("the last container is %s/%s; the rehearsal runs last because the operator "+
			"reads a terminated container's message and the rehearsal is the answer",
			pod.Containers[0].Name, pod.Containers[0].Image)
	}

	// The ship container must not be able to damage what it is copying.
	for _, m := range pod.InitContainers[1].VolumeMounts {
		if m.MountPath == "/backup" && !m.ReadOnly {
			t.Error("the ship container mounts the archives read-write; its whole job is to " +
				"copy them away, and nothing it does should be able to change them")
		}
	}
}

// The credential is a reference and never a value. Everything else on this
// object is readable with `kubectl get database -o yaml`, printed by describe,
// and written to git by the path that renders it.
func TestTheCredentialIsNowhereInTheRenderedObject(t *testing.T) {
	s3 := destination()
	job := desiredBackupCronJob(backedUp(s3))

	rendered, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), s3TestSecretValue) {
		t.Error("the S3 secret key is in the rendered CronJob; anyone who may read the kind " +
			"can read the credential, and the write path commits this object to git")
	}

	ship := job.Spec.JobTemplate.Spec.Template.Spec.InitContainers[1]
	for _, arg := range ship.Command {
		if strings.Contains(arg, "$S3_SECRET_ACCESS_KEY\"") && strings.Contains(arg, "alias set") {
			// Reading the variable is fine; passing it as an argument is not.
			if strings.Contains(arg, "alias set "+s3Alias+" \"$S3_ENDPOINT\" \"$S3_ACCESS_KEY_ID\"") {
				t.Error("the credential is an argument to the client, so it is in the process " +
					"table and in `kubectl describe pod`")
			}
		}
	}

	var refs int
	for _, e := range ship.Env {
		switch e.Name {
		case "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY":
			if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Errorf("%s carries a literal value rather than a secret reference", e.Name)
				continue
			}
			if e.ValueFrom.SecretKeyRef.Name != s3Secret {
				t.Errorf("%s reads from Secret %q, want %q", e.Name,
					e.ValueFrom.SecretKeyRef.Name, s3Secret)
			}
			refs++
		}
	}
	if refs != 2 {
		t.Errorf("the ship container takes %d secret references, want 2", refs)
	}
}

// --- the ship script, run the way the pod runs it ---------------------------

// fakeMC writes a stand-in client that records its arguments, copies files, and
// fails with whatever a case tells it to say.
//
// The failures it prints are not invented. Every one is a string this client
// produced against MinIO on 2026-09-05, captured by causing that failure on
// purpose — a wrong secret, a bucket that is not there, an endpoint that does
// not resolve, and a download into a filesystem too small to hold it.
func fakeMC(t *testing.T, dir, failOn, saying string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$ARGV"; done
mode=""
for a in "$@"; do
  case "$a" in
    "alias") mode=alias ;;
    "cp") mode=cp ;;
  esac
done
if [ "$mode" = cp ]; then
  n=$(cat "$ARGV.cp" 2>/dev/null || echo 0)
  n=$((n + 1))
  echo "$n" > "$ARGV.cp"
fi
# ONLY_CP pins the failure to one copy. Without it a case meaning to test the
# upload is satisfied by the download failing instead, which is how the guard
# for the success-line trap passed against the mutation it exists to catch.
if [ "$mode" = "` + failOn + `" ] && { [ -z "${ONLY_CP:-}" ] || [ "$mode" != cp ] || [ "$n" = "$ONLY_CP" ]; }; then
  printf '%s' "` + saying + `" >&2
  exit 1
fi
if [ "$mode" = cp ]; then
  # The two operands are the last two arguments.
  src=""; dst=""
  for a in "$@"; do src=$dst; dst=$a; done
  case "$dst" in
    dst/*) : ;;                       # an upload: nothing local to write
    *) cp "` + `${SRCFILE}` + `" "$dst" ;;   # a download: produce the archive
  esac
  case "$src" in *) : ;; esac
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "mc"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// shipEnv lays out the tree the pod gives the ship container and returns its
// environment.
func shipEnv(t *testing.T, bin string) map[string]string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"backup", "rehearsal", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(root, "backup", "app-20260905-020000.sql.gz")
	if err := os.WriteFile(archive, []byte("an archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	handoff := filepath.Join(root, "rehearsal", "run.env")
	if err := os.WriteFile(handoff, []byte(
		"ARCHIVE="+archive+"\nARCHIVE_SOURCE=local\nSIZE=10\nSRC_ROWS=3\nSTARTED=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"PATH":                 bin + ":" + os.Getenv("PATH"),
		"S3_ENDPOINT":          "https://minio.example.test:9000",
		"S3_BUCKET":            s3Bucket,
		"S3_PREFIX":            "shop",
		"S3_ACCESS_KEY_ID":     "AKIAEXAMPLE",
		"S3_SECRET_ACCESS_KEY": s3TestSecretValue,
		"ERR":                  filepath.Join(root, "tmp", "mc.err"),
		"RESULT":               filepath.Join(root, "result"),
		"ARGV":                 filepath.Join(root, "argv"),
		"SRCFILE":              archive,
		"HANDOFF":              handoff,
		"ROOT":                 root,
	}
}

// The claim this whole change exists to protect.
//
// docs/BACKUP-SURVEY.md says no platform you deploy with proves its backups
// restore. Uploading to S3 and then rehearsing the local file would keep the
// sentence and lose the thing: what is proven is the copy on the disk that just
// died, while the object anybody would actually reach for is proven of nothing.
// So the handoff the rehearsal reads has to end up pointing at what came back
// down out of the bucket.
func TestTheRehearsalIsPointedAtWhatCameBackFromS3(t *testing.T) {
	env := shipEnv(t, filepath.Join(t.TempDir(), "bin"))
	fakeMC(t, filepath.Dir(env["PATH"][:strings.Index(env["PATH"], ":")]+"/x"), "", "")

	script := strings.ReplaceAll(shipScript, handoffFile, env["HANDOFF"])
	script = strings.ReplaceAll(script, downloadedFile, filepath.Join(env["ROOT"], "rehearsal", "from-s3.sql.gz"))
	run := runScript(t, script, env)
	if run.code != 0 {
		t.Fatalf("the ship step failed: %d\n%s", run.code, run.stderr)
	}

	got, err := os.ReadFile(env["HANDOFF"])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	var archive, source string
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, "="); ok {
			switch k {
			case "ARCHIVE":
				archive = v
			case "ARCHIVE_SOURCE":
				source = v
			}
		}
	}
	if !strings.Contains(archive, "from-s3") {
		t.Errorf("the rehearsal was left pointing at %q; it has to restore what came back out "+
			"of the bucket, or the proof is about the local disk and the object in S3 is "+
			"proven of nothing", archive)
	}
	if want := "s3://" + s3Bucket + "/shop/app-20260905-020000.sql.gz"; source != want {
		t.Errorf("the rehearsal will record source %q, want %q; the status has to be able to "+
			"say which copy was read", source, want)
	}
}

// Four failures, four sentences, and never one that is true of all of them.
func TestEachWayShippingFailsGetsItsOwnSentence(t *testing.T) {
	for _, tc := range []struct {
		name, saying, reason, quote string
	}{
		{
			"a secret key the endpoint refuses",
			`{\"status\":\"error\",\"error\":{\"cause\":{\"message\":\"The request signature we calculated does not match the signature you provided.\",\"error\":{\"Code\":\"SignatureDoesNotMatch\"}}}}`,
			"credentials", "SignatureDoesNotMatch",
		},
		{
			"a bucket that is not there",
			`{\"status\":\"error\",\"error\":{\"cause\":{\"message\":\"Bucket \` + "`" + `archives\` + "`" + ` does not exist.\"}}}`,
			"bucket", "does not exist",
		},
		{
			"an endpoint that does not resolve",
			`{\"status\":\"error\",\"error\":{\"cause\":{\"message\":\"dial tcp: lookup nosuchhost: no such host\"}}}`,
			"network", "no such host",
		},
		{
			"a volume with no room on it",
			`{\"status\":\"error\",\"error\":{\"cause\":{\"message\":\"write /rehearsal/x: no space left on device\"}}}`,
			"disk", "no space left on device",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "bin")
			env := shipEnv(t, bin)
			fakeMC(t, bin, "cp", tc.saying)

			script := strings.ReplaceAll(shipScript, handoffFile, env["HANDOFF"])
			run := runScript(t, script, env)
			if run.code == 0 {
				t.Fatal("the ship step reported success after the client failed")
			}

			var res shipResult
			if err := json.Unmarshal([]byte(run.result), &res); err != nil {
				t.Fatalf("what it wrote is not parseable, so the operator reports nothing "+
					"at all (%v): %s", err, run.result)
			}
			if res.Shipped {
				t.Error("it reported the archive as shipped")
			}
			if res.Reason != tc.reason {
				t.Errorf("reason = %q, want %q; the four failures send four different people "+
					"to four different places", res.Reason, tc.reason)
			}
			if !strings.Contains(res.Message, tc.quote) {
				t.Errorf("the message is %q and does not carry the client's own words (%q); "+
					"a platform that paraphrases the tool is one nobody can debug against",
					res.Message, tc.quote)
			}
		})
	}
}

// The trap that made the exit code load-bearing, measured on 2026-09-05: with
// --json this client prints a success line describing what it is about to do
// BEFORE it tries, and for a bucket that does not exist that success line is
// also the last line it prints. A script reading either end of the output would
// call a failed upload a backup that shipped.
func TestASuccessLineAroundAFailureIsStillAFailure(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	env := shipEnv(t, bin)
	// Only the upload fails. Pinned, because a download failing afterwards
	// would satisfy this case without the upload guard existing at all.
	env["ONLY_CP"] = "1"
	fakeMC(t, bin, "cp",
		`{\"status\":\"success\",\"source\":\"/backup/x\"}\n`+
			`{\"status\":\"error\",\"error\":{\"cause\":{\"message\":\"Bucket \`+"`"+`archives\`+"`"+` does not exist.\"}}}\n`+
			`{\"status\":\"success\",\"total\":10,\"transferred\":0}`)

	// The download is redirected somewhere writable so that it genuinely
	// succeeds. Without this the mutation that swallows the upload failure is
	// caught by the download failing on a path that does not exist, which is
	// not the guard this case is about.
	script := strings.ReplaceAll(shipScript, handoffFile, env["HANDOFF"])
	script = strings.ReplaceAll(script, downloadedFile,
		filepath.Join(env["ROOT"], "rehearsal", "from-s3.sql.gz"))
	run := runScript(t, script, env)
	if run.code == 0 {
		t.Fatal("a failed copy surrounded by success lines was reported as a success; the " +
			"exit status is the only signal that means what it says")
	}
	var res shipResult
	if err := json.Unmarshal([]byte(run.result), &res); err != nil {
		t.Fatalf("unparseable result: %s", run.result)
	}
	if res.Reason != "bucket" {
		t.Errorf("reason = %q, want bucket", res.Reason)
	}
}

// A ship failure is not a rehearsal. Both write to the same termination path,
// and the reader that collects rehearsals must not turn one into the other —
// that would put "restored" on a page for a backup that never left the node.
func TestAShipFailureIsNeverReadAsARehearsal(t *testing.T) {
	const message = `{"finishedAt":"2026-09-05T02:00:00Z","shipped":false,` +
		`"reason":"bucket","message":"the bucket does not exist"}`
	terminated := func(name string) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			Name:  name,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}
	}

	// Asked where it can actually go wrong. The ship step runs as an init
	// container and the rehearsal reader only walks the regular ones, so
	// putting it there would make this case pass whether or not the reader
	// checks anything — which it did, until the mutation showed it.
	asRegular := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{terminated(shipContainer)},
	}}
	if got := rehearsalFromPod(asRegular); got != nil {
		t.Errorf("a failed upload was read as a rehearsal: %+v; that puts \"restored\" on a "+
			"page for a backup that never left the node", got)
	}

	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{terminated(shipContainer)},
	}}
	got := shipFailureFromPod(pod)
	if got == nil || got.Reason != "bucket" {
		t.Fatalf("the failure was not read back: %+v", got)
	}
}

// And the reasons the status reports are a fixed set, because a reason is what
// something else keys on and one arriving straight from a shell script is one a
// script change can rename without anything noticing.
func TestEveryShipReasonBecomesAConditionReason(t *testing.T) {
	for reported, want := range map[string]string{
		"credentials": "CredentialRefused",
		"bucket":      "BucketUnavailable",
		"network":     "EndpointUnreachable",
		"disk":        "NoSpace",
		"unknown":     "ShipFailed",
		"":            "ShipFailed",
	} {
		if got := shipReason(reported); got != want {
			t.Errorf("shipReason(%q) = %q, want %q", reported, got, want)
		}
	}
}
