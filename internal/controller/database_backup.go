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
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// backupSuffix keeps the backup objects out of the way of the server's own.
const (
	backupSuffix = "-backup"

	// The label value that finds the pods a backup left behind, and the
	// container inside them. Both are read back by latestRehearsal, so the
	// renderer and the reader have to spell them the same way.
	backupComponent = "database-backup"
	backupContainer = "dump"

	// The two extra containers a backup with an S3 destination runs. The work
	// splits three ways because no image can do all of it: pg_dump and initdb
	// live in the database's own image, and an S3 client does not.
	shipContainer     = "ship"
	rehearseContainer = "rehearse"
	handoffFile       = "/rehearsal/run.env"
	downloadedFile    = "/rehearsal/from-s3.sql.gz"
	localArchiveTag   = "local"

	// mcImage is the S3 client.
	//
	// Measured on 2026-09-05 against MinIO before it was chosen, because three
	// things had to be true and none of them is on a README: it has /bin/sh, it
	// runs as uid 70 under a read-only root filesystem with a writable /tmp for
	// --config-dir, and it takes its credentials from MC_HOST_<alias> in the
	// environment rather than from a flag. The last one is the one that
	// mattered: an access key in argv is in the process table and in
	// `kubectl describe pod`, which is the reason the control plane takes
	// -git-token-file rather than -git-token.
	//
	// What it does not have, also measured: sed, grep and awk. The
	// classification below is shell `case` for that reason, not for taste.
	mcImage = "minio/mc:RELEASE.2025-08-13T08-35-41Z"

	// The two keys the credentials Secret has to carry, spelled once because
	// the operator reads them and the user writes them and nothing checks that
	// the two agree until a backup fails at two in the morning.
	//
	// Prefixed rather than named accessKeyID, so one Secret can hold these
	// beside a database password without either shadowing the other — the
	// backup container already takes envFrom on the database's own Secret.
	s3AccessKeyIDKey     = "s3AccessKeyID"
	s3SecretAccessKeyKey = "s3SecretAccessKey"

	// offsiteCondition is where a Database says whether its archives leave the
	// cluster. Separate from Ready, which is about the server answering, and
	// from LastRestore, which is about an archive restoring.
	offsiteCondition = "BackupOffsite"

	// s3Alias is what the destination is called inside the ship container. It
	// is the suffix of the MC_HOST_ variable that carries the credential, so it
	// is spelled once here and read twice below.
	s3Alias = "dst"
)

func backupName(db *platformv1alpha1.Database) string { return db.Name + backupSuffix }

// desiredBackupClaim is where archives live.
//
// A volume of its own and not the data volume, so that filling it with backups
// cannot stop the database accepting writes — which would be a backup policy
// that causes the outage it exists to recover from.
func desiredBackupClaim(db *platformv1alpha1.Database) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: backupName(db), Namespace: db.Namespace, Labels: databaseLabels(db),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: db.Spec.Backup.Storage},
			},
		},
	}
	if db.Spec.StorageClass != "" {
		claim.Spec.StorageClassName = ptr.To(db.Spec.StorageClass)
	}
	return claim
}

// RestoreResultPath is where the rehearsal writes its answer for the operator
// to read.
//
// The termination log, and not the backup volume it also writes to. The job
// runs with no service-account token by design — PostgreSQL tooling has no
// business holding one — so it cannot write a status, and the operator cannot
// read a volume mounted by somebody else's pod. What Kubernetes already carries
// between the two is this file: whatever a container writes here appears in its
// terminated state, which the operator is watching anyway.
//
// The default path, stated because the manifest below has to agree with it and
// a default that moves would be a silent loss of the only channel there is.
const RestoreResultPath = "/dev/termination-log"

// backupScript dumps, and then restores what it dumped.
//
// Absorbed from chart/templates/backup-verify-job.yaml, where it was proven
// against a real cluster in CI. Two flags carry the whole proof and neither is
// decoration:
//
// ON_ERROR_STOP, because psql exits zero after every statement in a file has
// failed — without it the rehearsal is a slower way of asserting that the
// archive decompresses, which is the check it exists to replace.
//
// pipefail, because the exit status of "pg_dump | gzip" is gzip's, and gzip
// succeeds on the partial output of a pg_dump that died. That is how a
// truncated dump becomes a backup passing every check made on the file.
//
// The restore runs against a PostgreSQL this job starts in its own container,
// on a socket, in a directory that dies with the pod. Not a throwaway database
// on the live server: a rehearsal that runs on a schedule must not be able to
// damage the thing it rehearses for.
const dumpScript = `set -eu
set -o pipefail

until pg_isready -h "$DB_HOST" -U "$POSTGRES_USER" -d "$POSTGRES_DB" -q; do sleep 2; done

count_rows() {
  _rows=0
  for _t in $(eval "$1 -At -c \"SELECT schemaname||'.'||tablename FROM pg_tables
      WHERE schemaname NOT IN ('pg_catalog','information_schema')\""); do
    _n=$(eval "$1 -At -c \"SELECT count(*) FROM $_t\"")
    _rows=$((_rows + _n))
  done
  echo "$_rows"
}

SRC="psql -h $DB_HOST -U $POSTGRES_USER -d $POSTGRES_DB -v ON_ERROR_STOP=1 -q"
SRC_ROWS=$(count_rows "$SRC")

STARTED=$(date -u +%Y-%m-%dT%H:%M:%SZ)
OUT="/backup/${POSTGRES_DB}-$(date +%Y%m%d-%H%M%S).sql.gz"
pg_dump -h "$DB_HOST" -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
  --no-owner --no-privileges | gzip > "$OUT"
gzip -t "$OUT"
SIZE=$(wc -c < "$OUT")
[ "$SIZE" -gt 100 ] || { echo "backup suspiciously small: $SIZE bytes"; exit 1; }

find /backup -name '*.sql.gz' -mtime "+${RETAIN_DAYS}" -delete

# Handed to whatever rehearses, which is this same shell when the archives stay
# on the volume and a container two steps later when they do not.
#
# Written on both paths rather than only on the one that needs it. The
# alternative is two spellings of the same phase, and a shell script is a joint
# nothing type-checks: the day one of them grows a field the other does not, the
# reader of the missing one sees a zero and reports it as an answer.
mkdir -p /rehearsal
printf 'ARCHIVE=%s\nARCHIVE_SOURCE=%s\nSIZE=%s\nSRC_ROWS=%s\nSTARTED=%s\n' \
  "$OUT" "` + localArchiveTag + `" "$SIZE" "$SRC_ROWS" "$STARTED" > ` + handoffFile + `
`

// rehearseScript restores an archive into a throwaway instance and counts what
// came back.
//
// It reads which archive from the handoff rather than deciding, and that
// indirection is the whole of this change rather than a detail of it. When the
// archives go to S3 the file this restores is the one that was pulled back down
// out of the bucket — so what is proven is the object somebody would actually
// reach for, not the copy sitting on the disk that just died.
const rehearseScript = `set -eu
set -o pipefail

. ` + handoffFile + `

count_rows() {
  _rows=0
  for _t in $(eval "$1 -At -c \"SELECT schemaname||'.'||tablename FROM pg_tables
      WHERE schemaname NOT IN ('pg_catalog','information_schema')\""); do
    _n=$(eval "$1 -At -c \"SELECT count(*) FROM $_t\"")
    _rows=$((_rows + _n))
  done
  echo "$_rows"
}

if [ "${REHEARSE:-true}" != "true" ]; then
  echo "backed up $ARCHIVE ($SIZE bytes); no rehearsal was asked for"
  printf '{"finishedAt":"%s","archive":"%s","archiveBytes":%s,"source":"%s","restored":false}' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$ARCHIVE" "$SIZE" "$ARCHIVE_SOURCE" > ` + RestoreResultPath + `
  exit 0
fi

export PGDATA=/rehearsal/data
initdb --username=rehearsal --auth=trust >/dev/null
pg_ctl -o "-k /tmp -c listen_addresses=''" -w -l /tmp/pg.log start >/dev/null
trap 'pg_ctl -w stop >/dev/null 2>&1 || true' EXIT

PSQL="psql -h /tmp -U rehearsal -v ON_ERROR_STOP=1 -q"
$PSQL -d postgres -c "CREATE DATABASE rehearsal" >/dev/null
gunzip -c "$ARCHIVE" | $PSQL -d rehearsal >/dev/null

TABLES=$($PSQL -At -d rehearsal -c \
  "SELECT schemaname||'.'||tablename FROM pg_tables
    WHERE schemaname NOT IN ('pg_catalog','information_schema')")
[ -n "$TABLES" ] || { echo "the restore produced no tables"; exit 1; }
TABLE_COUNT=$(printf '%s\n' $TABLES | wc -l | tr -d ' ')
ROWS=$(count_rows "$PSQL -d rehearsal")

if [ "$SRC_ROWS" -gt 0 ] && [ "$ROWS" -eq 0 ]; then
  echo "the source holds $SRC_ROWS rows and the restore holds none:"
  echo "the archive is readable and carries no data"
  exit 1
fi

FINISHED=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "restored $ARCHIVE (from $ARCHIVE_SOURCE) into a private instance: $TABLE_COUNT tables, $ROWS rows"

# Written twice on purpose. The volume keeps the history; the termination log is
# the only thing the operator can read, and it holds one run.
printf '{"startedAt":"%s","finishedAt":"%s","archive":"%s","archiveBytes":%s,"tables":%s,"rows":%s,"sourceRows":%s,"source":"%s","restored":true}' \
  "$STARTED" "$FINISHED" "$ARCHIVE" "$SIZE" "$TABLE_COUNT" "$ROWS" "$SRC_ROWS" "$ARCHIVE_SOURCE" \
  > /backup/last-restore.json
cat /backup/last-restore.json > ` + RestoreResultPath + `
`

// shipScript copies the archive to S3 and pulls it back down again.
//
// Both halves, and the second is the point. Uploading and then rehearsing the
// local file proves that the copy on this disk restores — which was already
// proven — while the object somebody would reach for after losing the node has
// been proven of nothing. So the archive is fetched back out of the bucket and
// the rehearsal restores that, and RestoreRehearsal.Source says which was read.
//
// The credential arrives on stdin rather than in the environment or in argv.
// Argv is out for the reason the control plane takes -git-token-file: it is in
// the process table and in `kubectl describe pod`. The MC_HOST_<alias> URL was
// measured working on 2026-09-05 even for a secret containing / and +, but it
// is a URL, and the one shape of secret that would break it silently is the one
// nobody tests with. `alias set` reading two lines off stdin parses nothing.
const shipScript = `set -eu

. ` + handoffFile + `

MC="mc --config-dir /tmp/mc"
ERR=${ERR:-/tmp/mc.err}
# Read from the environment with the real default, the way every other script
# here is: a rule nothing executes is decoration, and this is what lets the
# classification below be run against the strings the client really produced
# rather than asserted about by reading it.
RESULT=${RESULT:-` + RestoreResultPath + `}

# Four sentences, and never one that is true of all of them.
#
# Every pattern below is a string this client actually produced, captured on
# 2026-09-05 against MinIO by causing each failure on purpose. No sed, grep or
# awk: the image has none of the three, which is also measured.
say() {
  printf '{"finishedAt":"%s","shipped":false,"reason":"%s","message":"%s"}' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" > "$RESULT"
  echo "$2" >&2
  exit 1
}

classify() {
  _what=$1
  _said=$(tail -c 600 "$ERR" 2>/dev/null | tr '\n"' '  ')
  case "$_said" in
    *SignatureDoesNotMatch*|*InvalidAccessKeyId*|*AccessDenied*)
      say credentials "the S3 credential in the secret was refused while trying to $_what: $_said" ;;
    *"does not exist"*|*NoSuchBucket*)
      say bucket "the bucket does not exist or this credential cannot see it, trying to $_what: $_said" ;;
    *"no such host"*|*"connection refused"*|*"i/o timeout"*|*"context deadline exceeded"*|*"no route to host"*)
      say network "the S3 endpoint could not be reached to $_what: $_said" ;;
    *"no space left on device"*)
      say disk "there is no room left on the volume to $_what: $_said" ;;
    *)
      # Not a fallback that invents a reason. The client said something this
      # has not been taught, and quoting it is strictly better than naming one
      # of the four above and being wrong about which.
      say unknown "the S3 client refused to $_what and said: $_said" ;;
  esac
}

# Two lines on stdin: the access key, then the secret. Neither is ever an
# argument, so neither is in the process table.
printf '%s\n%s\n' "$S3_ACCESS_KEY_ID" "$S3_SECRET_ACCESS_KEY" \
  | $MC alias set ` + s3Alias + ` "$S3_ENDPOINT" >/dev/null 2>"$ERR" \
  || classify "configure the S3 endpoint $S3_ENDPOINT"

KEY="${S3_PREFIX:+$S3_PREFIX/}$(basename "$ARCHIVE")"
REMOTE="` + s3Alias + `/$S3_BUCKET/$KEY"

# --json is asked for so classify() has the client's own words, and the exit
# status is what decides success. Measured, and this is the trap: with --json
# the client prints a {"status":"success"} line describing what it is about to
# do BEFORE it tries, and for a bucket that does not exist that success line is
# also the LAST line printed. A script reading either the first line or the last
# would report a failed upload as a backup that shipped.
$MC --json cp "$ARCHIVE" "$REMOTE" >"$ERR" 2>&1 || classify "upload to s3://$S3_BUCKET/$KEY"

# And back down, into the volume that dies with this pod rather than over the
# archive that was just uploaded.
$MC --json cp "$REMOTE" "` + downloadedFile + `" >"$ERR" 2>&1 \
  || classify "read back s3://$S3_BUCKET/$KEY"

[ -s "` + downloadedFile + `" ] \
  || say unknown "s3://$S3_BUCKET/$KEY was written and came back empty"

# Appended, not rewritten. The rehearsal sources this file top to bottom, so
# these two win over the ones the dump wrote — which is how the phase that
# proves the backup is pointed at the remote copy without either script
# knowing whether the other ran.
printf 'ARCHIVE=%s\nARCHIVE_SOURCE=%s\n' "` + downloadedFile + `" "s3://$S3_BUCKET/$KEY" >> ` + handoffFile + `
echo "shipped s3://$S3_BUCKET/$KEY and read it back"
`

// backupScript is the shape that has always run: dump and rehearse in one
// container, because with no S3 destination there is nothing between them.
const backupScript = dumpScript + "\n" + rehearseScript

// desiredBackupCronJob renders the schedule.
func desiredBackupCronJob(db *platformv1alpha1.Database) *batchv1.CronJob {
	rehearse := "true"
	if db.Spec.Backup.Rehearse != nil && !*db.Spec.Backup.Rehearse {
		rehearse = "false"
	}

	job := backupCronJob(db, rehearse)
	if db.Spec.Backup.S3 != nil {
		splitForS3(&job.Spec.JobTemplate.Spec.Template.Spec, db)
	}
	return job
}

// splitForS3 turns the one container this has always run into the three it
// takes once the archives leave the cluster.
//
// Three because no image can do all of it. pg_dump and initdb live in the
// database's own image and an S3 client does not, and the order is dump, ship,
// restore — so the phases alternate between two images and cannot be a sidecar.
// Init containers run to completion in order, which is exactly that.
//
// The two PostgreSQL containers are copies of the one that was already there,
// with a different script. Copied rather than rebuilt so that everything the
// original decided — the security context, the resource bounds, the secret it
// reads its password from, the volumes — stays decided in one place.
func splitForS3(pod *corev1.PodSpec, db *platformv1alpha1.Database) {
	base := pod.Containers[0]

	dump := *base.DeepCopy()
	dump.Name = backupContainer
	dump.Command = []string{"sh", "-c", dumpScript}

	restore := *base.DeepCopy()
	restore.Name = rehearseContainer
	restore.Command = []string{"sh", "-c", rehearseScript}

	// Ordered dump, ship, and the rehearsal last. Last because the operator
	// reads a terminated container's message and the rehearsal is the answer;
	// the ship container writes one only when it has failed, which is a
	// different sentence and is read separately.
	pod.InitContainers = []corev1.Container{dump, shipSide(db, base)}
	pod.Containers = []corev1.Container{restore}
}

// shipSide is the container that talks to S3.
func shipSide(db *platformv1alpha1.Database, base corev1.Container) corev1.Container {
	s3 := db.Spec.Backup.S3
	endpoint := s3.Endpoint
	if endpoint == "" {
		// AWS, which is the only implementation whose address is not
		// configuration. Spelled here rather than defaulted in the API,
		// because a default in the API is a value a reader has to know is not
		// what they typed.
		endpoint = "https://s3.amazonaws.com"
	}
	fromSecret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: s3.CredentialsSecret},
			Key:                  key,
		}}
	}

	return corev1.Container{
		Name:    shipContainer,
		Image:   mcImage,
		Command: []string{"sh", "-c", shipScript},
		Env: []corev1.EnvVar{
			{Name: "S3_ENDPOINT", Value: endpoint},
			{Name: "S3_BUCKET", Value: s3.Bucket},
			{Name: "S3_PREFIX", Value: s3.Prefix},
			// The two that are not values. They arrive as references, so the
			// key is in a Secret and not in this object, and the script hands
			// them to the client on stdin rather than in argv.
			{Name: "S3_ACCESS_KEY_ID", ValueFrom: fromSecret(s3AccessKeyIDKey)},
			{Name: "S3_SECRET_ACCESS_KEY", ValueFrom: fromSecret(s3SecretAccessKeyKey)},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			// Measured before it was asserted: this client runs as uid 70 with
			// a read-only root filesystem, given a writable /tmp to put its
			// --config-dir in.
			ReadOnlyRootFilesystem: ptr.To(true),
			Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}},
		},
		TerminationMessagePath:   RestoreResultPath,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Resources:                base.Resources,
		VolumeMounts: []corev1.VolumeMount{
			// Read-only. This container's job is to copy the archive away, and
			// nothing it does should be able to damage the archive.
			{Name: "backup", MountPath: "/backup", ReadOnly: true},
			{Name: tmpVolume, MountPath: tmpPath},
			{Name: "rehearsal", MountPath: "/rehearsal"},
		},
	}
}

func backupCronJob(db *platformv1alpha1.Database, rehearse string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: backupName(db), Namespace: db.Namespace, Labels: databaseLabels(db),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: db.Spec.Backup.Schedule,
			// Never two dumps at once. A second one starting while the first
			// still holds the volume is how a backup window turns into a
			// half-written archive.
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: ptr.To(int32(3)),
			FailedJobsHistoryLimit:     ptr.To(int32(3)),
			// A missed window is worth catching up on for ten minutes and not
			// for ever: a controller that was down overnight should take one
			// backup on the way up, not queue every one it slept through.
			StartingDeadlineSeconds: ptr.To(int64(600)),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit:          ptr.To(int32(2)),
					ActiveDeadlineSeconds: ptr.To(int64(3600)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
							nameLabel:      postgresPortName,
							instanceLabel:  db.Name,
							componentLabel: backupComponent,
						}},
						Spec: corev1.PodSpec{
							RestartPolicy:                corev1.RestartPolicyOnFailure,
							AutomountServiceAccountToken: ptr.To(false),
							SecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot:   ptr.To(true),
								RunAsUser:      ptr.To(postgresUID),
								RunAsGroup:     ptr.To(postgresUID),
								FSGroup:        ptr.To(postgresUID),
								SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
							},
							Containers: []corev1.Container{{
								Name:  backupContainer,
								Image: db.Spec.Image,
								SecurityContext: &corev1.SecurityContext{
									AllowPrivilegeEscalation: ptr.To(false),
									ReadOnlyRootFilesystem:   ptr.To(true),
									Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}},
								},
								Command: []string{"sh", "-c", backupScript},
								EnvFrom: []corev1.EnvFromSource{{
									SecretRef: &corev1.SecretEnvSource{
										LocalObjectReference: corev1.LocalObjectReference{Name: db.Name},
									},
								}},
								Env: []corev1.EnvVar{
									{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: db.Name},
											Key:                  "POSTGRES_PASSWORD",
										},
									}},
									{Name: "RETAIN_DAYS", Value: strconv.Itoa(int(db.Spec.Backup.RetainDays))},
									{Name: "REHEARSE", Value: rehearse},
								},
								// Stated rather than defaulted, because the
								// script writes to this exact path and a
								// default that moved would silently take away
								// the only channel between this pod and the
								// operator.
								TerminationMessagePath: RestoreResultPath,
								// The message is JSON the operator parses, so it
								// has to be what the container wrote and not
								// the last lines of its log.
								TerminationMessagePolicy: corev1.TerminationMessageReadFile,
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    db.Spec.Resources.CPURequest,
										corev1.ResourceMemory: db.Spec.Resources.MemoryRequest,
									},
									Limits: corev1.ResourceList{
										corev1.ResourceMemory: db.Spec.Resources.MemoryLimit,
									},
								},
								VolumeMounts: []corev1.VolumeMount{
									{Name: "backup", MountPath: "/backup"},
									{Name: tmpVolume, MountPath: tmpPath},
									{Name: "rehearsal", MountPath: "/rehearsal"},
								},
							}},
							Volumes: []corev1.Volume{
								{Name: "backup", VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: backupName(db),
									},
								}},
								{Name: tmpVolume, VolumeSource: corev1.VolumeSource{
									EmptyDir: &corev1.EmptyDirVolumeSource{},
								}},
								// The restore target, and deliberately not the
								// backup volume: a rehearsal that writes its
								// scratch instance beside the archives it is
								// verifying can fill the volume they live on,
								// which turns a failed rehearsal into a failed
								// backup.
								{Name: "rehearsal", VolumeSource: corev1.VolumeSource{
									EmptyDir: &corev1.EmptyDirVolumeSource{},
								}},
							},
						},
					},
				},
			},
		},
	}
}
