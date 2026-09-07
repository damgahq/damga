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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

const (
	// BuildResultPath is where the build writes its answer. The same channel
	// the restore rehearsal uses, for the same reason: a job with no service
	// account token has one way to speak to the control plane, and giving a
	// build container an API identity would hand that identity to whatever it
	// is compiling.
	BuildResultPath = "/dev/termination-log"

	buildContainer = "build"

	// prepareContainer clones and decides. It only exists when the buildpack
	// path is possible, because that path needs a second image and the
	// decision needs the working tree.
	prepareContainer = "prepare"

	buildComponent = "build"

	// buildkitImage builds from a Dockerfile without a Docker daemon.
	//
	// Not the rootless variant, which was tried first and does not work on the
	// hosts this targets: Ubuntu 23.10 and later restrict unprivileged user
	// namespaces through AppArmor, so rootlesskit cannot start its child and
	// buildkitd never comes up. That is a host kernel policy and no pod setting
	// reaches it. cluster/build-namespace.yaml carries the full reasoning and
	// the boundary that replaces the pod-level one.
	buildkitImage = "moby/buildkit:v0.27.0"

	// builderImage is where the Cloud Native Buildpacks lifecycle lives, and it
	// has to be a second container rather than a second toolchain: the
	// lifecycle does not just call the buildpacks, it runs them *in* this
	// image's filesystem, so the build base and the buildpacks are the
	// container. buildkit's image has no /cnb at all.
	//
	// Measured from the image config on 2026-09-01 rather than assumed, because
	// three of the numbers below are load-bearing and two of them are not what
	// a reader would guess:
	//
	//   lifecycle 0.21.18, platform APIs 0.7 through 0.15, none deprecated
	//   User 1001:1000 (CNB_USER_ID=1001, CNB_GROUP_ID=1000) — jammy, not 1000
	//   linux/amd64 only: the tag resolves to one manifest, not an index
	//   1267 MB of compressed layers, against buildkit's 101 MB
	//
	// The third is a limitation nothing here enforces: on an arm64 node this
	// pod cannot start, and neither the API nor the status says why beforehand.
	// The fourth is why the pod shape below depends on what was asked for.
	//
	// -base rather than -full, whose extra 393 MB buys Ruby and PHP. Base's own
	// description lists Java, Go, .NET Core, Node.js, Python, Apache HTTPD,
	// NGINX and Procfile — so "every language" is already not true here, and
	// the two the brief named that are missing are exactly those two.
	builderImage = "paketobuildpacks/builder-jammy-base:0.4.630"

	// platformAPI is the contract version the phases are asked to speak.
	//
	// Pinned rather than left to default, because a lifecycle with no
	// CNB_PLATFORM_API in its environment picks for itself and the choice
	// changes what the flags below mean. 0.13 sits inside the window the
	// pinned builder supports (0.7-0.15, measured) with room on both sides, so
	// a builder bumped forward or nudged back still speaks it.
	platformAPI = "0.13"

	// preparedPath is where the second container sees the tree the first one
	// cloned. Deliberately not /workspace: in the builder image /workspace is
	// the application directory the lifecycle builds from, it is owned by the
	// buildpack user, and a volume mounted there would cover it — the same
	// trap emptyDir has already sprung once in this repository.
	preparedPath = "/prepared"

	// workspaceVolume carries the checkout from the container that clones to
	// the container that builds.
	workspaceVolume = "workspace"

	// The three inputs both containers need: where the result is pushed, where
	// the layers worth keeping are kept, and which directory of the repository
	// is the application.
	envImage      = "IMAGE"
	envCacheImage = "CACHE_IMAGE"
	envPathInRepo = "PATH_IN_REPO"
	envRepo       = "REPO"
	envRevision   = "REVISION"
	envMethod     = "METHOD"

	// And the paths both scripts read with a real default, so a test can point
	// them somewhere and run the thing rather than assert about its text.
	envWorkspace = "WORKSPACE"
	envResult    = "RESULT"

	// The variable the platform specification lists as a base-image one that
	// SHOULD be in the lifecycle's environment; see buildpackHome.
	envHome = "HOME"

	// cacheTag is where a repository's reusable layers live, in the registry,
	// beside the images built from it.
	//
	// A registry reference and not a volume, and that is the design rather than
	// a preference. The obvious alternative is a PersistentVolumeClaim the
	// builds share, and it was measured before it was rejected — because the
	// reason to reject it is not the one it looks like.
	//
	// Measured 2026-09-01, the three-node kind cluster this repository runs,
	// ten Jobs mounting one ReadWriteOnce claim, which is what the build
	// namespace's count/jobs.batch of 10 permits:
	//
	//   all ten pods were scheduled, all ten ran, all ten Completed
	//   all ten landed on one node, and all ten mounted the claim read-write
	//   each of the ten appended a line and then counted the file: all read 10
	//   no Multi-Attach, no Pending pod, no warning event at all
	//
	// So ReadWriteOnce does not serialise anything here. It is one *node*, not
	// one pod, and the scheduler co-locates the pods precisely because the
	// volume is node-bound — which turns the guard into its opposite: ten
	// builds writing one cache directory at once, silently, and a build queue
	// pinned to whichever node holds the volume. A corrupt cache from that
	// reads later as a broken build, and nothing in the ten pods' output says
	// where it came from.
	//
	// The registry has neither problem. Ten builds read one cache concurrently
	// because a pull is a read; two builds of the same repository finishing at
	// once push the same reference and the last writer wins, which costs the
	// loser's cache and nothing else. It puts no build on any particular node.
	// And it survives the pod, which is the whole point: the disk this job has
	// dies with it.
	//
	// It cannot collide with an image this build pushes. buildImageRef tags
	// with the revision, which the CRD pins to forty hex characters, and this
	// is a word.
	cacheTag = "buildcache"

	// buildpackHome is set because the platform specification lists HOME as a
	// base-image variable that SHOULD be in the lifecycle's environment and is
	// inherited by every buildpack unmodified — and the builder image's config
	// does not set one (measured: its env is PATH, CNB_USER_ID, CNB_GROUP_ID,
	// CNB_STACK_ID). Whether the runtime would supply one anyway is not
	// measured, which is the reason to set it rather than to find out from a
	// buildpack failing somewhere inside itself. /home/cnb exists in the image
	// and belongs to the buildpack user.
	buildpackHome = "/home/cnb"
)

// resultPrelude is the half of both build scripts that talks to the control
// plane, shared so the two cannot drift: the field names here are the field
// names in buildResult, and a shell script is a joint nothing type-checks.
//
// Written to the termination log on both paths. A build that succeeds and says
// nothing is indistinguishable from one that never ran, and the digest is the
// entire product of this job — the tag is a name that can be moved and the
// digest is what goes into the Workload.
//
// RESULT is read from the environment so a test can point it at a file and run
// the script for real. It defaults to the same path the pod spec sets, so the
// container behaves identically whether or not anybody set it.
const resultPrelude = `set -eu

RESULT=${RESULT:-` + BuildResultPath + `}
RAN=unknown
SAID=

say() {
  SAID=1
  printf '{"method":"%s","message":%s}' "$RAN" \
    "$(printf '%s' "$1" | head -c 900 | sed 's/\\/\\\\/g; s/"/\\"/g' \
       | tr '\n\t' '  ' | tr -d '\000-\037' | sed 's/^/"/; s/$/"/')" \
    > "$RESULT"
}

fail() { say "$1"; exit 1; }

# Anything that exits without having written a result writes one here.
#
# Added because the first real build died on a git command that had no || fail
# after it, so set -e killed the script before anything reached the termination
# log. The control plane then had only the Job's own condition — "backoff limit
# exceeded" — while the actual reason sat in a pod log nobody was reading. A
# handler that only covers the failures it anticipated is a handler for the
# failures that do not happen.
# Guarded on SAID, because fail() writes and then exits — which fires this trap,
# which would otherwise overwrite the specific message with the generic one. The
# first version did exactly that, and the only reason it was not shipped is that
# reading it back was cheaper than watching it run.
trap 'rc=$?; [ "$rc" = 0 ] || [ -n "$SAID" ] || say "the build exited with status $rc; see the job log"' EXIT

# Inputs are checked with an explicit test and not with ${VAR:?}, which is what
# this did until the check for the cache reference was added to it and the new
# guard was found not to guard.
#
# Measured 2026-09-01, the three failure shapes this script can take, under the
# trap above:
#
#   fail "..."                    exit 1, its own message written
#   any command failing, set -e   exit 1, the trap's generic message written
#   : "${VAR:?}"                  exit 0, nothing written at all
#
# The third is the one that matters and it is the one this file exists to
# prevent. A shell aborted by ${VAR:?} runs its EXIT trap with $? still 0, so
# the trap concludes nothing went wrong; the shell then exits 0, the container
# succeeds carrying an empty termination message, and a build that never
# started is indistinguishable from one that did. The message goes to the pod
# log, which is the place the control plane does not read.
#
# need() routes the same question through fail(), which is the shape that was
# measured to work.
need() { [ -n "$2" ] || fail "the build was started with no $1"; }
need IMAGE "${IMAGE:-}"
`

// buildScript clones, decides how to build, and builds from a Dockerfile.
//
// One script rather than two containers for the decision, because the decision
// needs the working tree: "is there a Dockerfile" is answerable only after the
// clone, and a controller that guessed beforehand would be guessing about
// somebody else's repository.
//
// When the answer is a buildpack it stops here and says nothing at all. The
// lifecycle cannot run in this image, so the container after this one finishes
// the job — and a termination message written here would be the one the control
// plane reads, because it reads the first container that left a parseable one.
const buildScript = resultPrelude + `
need REPO "${REPO:-}"
need REVISION "${REVISION:-}"
need METHOD "${METHOD:-}"
need CACHE_IMAGE "${CACHE_IMAGE:-}"

WORKSPACE=${WORKSPACE:-/workspace}
# Read from the environment with the real default, for the reason the buildpack
# script already does it with every path it touches: a rule nothing executes is
# decoration, and these two are what a test needs in order to run this branch
# for real rather than assert that a string contains a flag.
META=${META:-/tmp/meta.json}
BUILD_ERR=${BUILD_ERR:-/tmp/build.err}

# A subdirectory the container creates itself, not the mount point.
#
# /workspace is an emptyDir and belongs to root; git refuses to operate on a
# repository whose directory somebody else owns ("detected dubious ownership")
# and this container is uid 1000. Creating the directory here makes the
# container its owner, which is the answer git is actually asking for —
# safe.directory would tell git to stop checking instead.
mkdir -p "$WORKSPACE/src"
cd "$WORKSPACE/src"


git init -q . || fail "could not initialise a repository"
git remote add origin "$REPO" || fail "could not set the remote to $REPO"
# One commit, not a history. A shallow fetch of the exact revision is the
# difference between seconds and minutes on a repository with any age, and
# nothing here reads the log.
git fetch -q --depth 1 origin "$REVISION" || fail "could not fetch $REVISION from $REPO"
git checkout -q FETCH_HEAD || fail "could not check out $REVISION"

SRC="$WORKSPACE/src${PATH_IN_REPO:+/$PATH_IN_REPO}"
[ -d "$SRC" ] || fail "the path $PATH_IN_REPO does not exist in this repository"

RAN="$METHOD"
if [ "$METHOD" = detect ]; then
  if [ -f "$SRC/Dockerfile" ]; then RAN=dockerfile; else RAN=buildpack; fi
fi
# The decision, written where the next container can read it. It cannot repeat
# the detection itself: it sees the tree read-only, and two answers to one
# question is one answer too many.
printf '%s' "$RAN" > "$WORKSPACE/method"

case "$RAN" in
  dockerfile)
    [ -f "$SRC/Dockerfile" ] || fail "builder is dockerfile but this repository has none"
    # stderr kept rather than discarded. The first run of this reported "the
    # Dockerfile build failed", which is true of every failure and says nothing;
    # the actual reason — a directory that was still read-only — was in a pod
    # log the control plane never reads. A build fails for reasons that belong
    # to the user's code, so the user's own tool has to be the one talking.
    # The two cache flags, against one reference: the layers this build may
    # reuse, and the layers the next build of this repository will.
    #
    # Measured on 2026-09-01 against this repository's own app/, pushing to a
    # registry in a container, with a throwaway buildkit for every run because
    # every build in the cluster is a new pod:
    #
    #   cold, no cache flags                    26s
    #   the identical build again, no flags     19s   npm ci ran again
    #   these flags, cache reference absent     21s   exit 0, see below
    #   these flags, cache warm                  6s   npm ci reported CACHED
    #
    # The third line is the one that had to be checked rather than assumed. A
    # missing cache reference prints
    #
    #   ERROR: failed to configure registry cache importer: ... not found
    #
    # into the build log and the build still succeeds — measured, exit 0, image
    # and cache both pushed. So the first build of a repository carries a line
    # that reads like a failure and is not, and nothing here should treat it as
    # one: fail() fires on a non-zero exit, and this exit is zero.
    #
    # mode=max rather than the default min, and type=inline not at all. Both of
    # those keep only the layers that end up *in* the pushed image, and the
    # dependency install this exists for is a stage that does not: app/ runs
    # npm ci in stage one and copies node_modules out in stage two, which is
    # the ordinary shape of a Dockerfile and exactly the shape a min-mode cache
    # stores nothing useful for.
    #
    # ignore-error on the export, because the cache is an optimisation and the
    # image is the product. A registry that takes the image and refuses the
    # cache — out of space, a retention rule, a pull-through mirror that is
    # read-only — must not turn a build that worked into a build that failed.
    #
    # And the import is safe on the first build of a repository, when the
    # reference does not exist: measured in the same run, exit 0.
    # The build-time variables, turned into flags here rather than composed in
    # Go. What is passed into this container is a list of names and one
    # variable per value, and the reason is that the alternative is a string
    # somebody has to quote correctly: a value with a space, a quote or a
    # newline in it would be split by the shell on the way past.
    #
    # printenv rather than eval. eval on a name that came from a user is the
    # shape of every shell injection ever written, and the name here reaches
    # this line from a form. printenv takes the name as data.
    #
    # An unset value is an empty string and not an error: the API refuses a
    # nameless variable and allows a valueless one, and a build-arg with no
    # value is a legitimate way to unset one the Dockerfile defaults.
    set --
    for name in ${BUILD_ARGS:-}; do
      set -- "$@" --opt "build-arg:${name}=$(printenv "BUILD_ARG_${name}" || true)"
    done

    buildctl-daemonless.sh build "$@" \
      --frontend dockerfile.v0 \
      --local context="$SRC" --local dockerfile="$SRC" \
      --output "type=image,name=${IMAGE},push=true,registry.insecure=true" \
      --import-cache "type=registry,ref=${CACHE_IMAGE},registry.insecure=true" \
      --export-cache "type=registry,ref=${CACHE_IMAGE},mode=max,ignore-error=true,registry.insecure=true" \
      --metadata-file "$META" 2>"$BUILD_ERR" \
      || fail "$(tail -c 700 "$BUILD_ERR")"
    # Whitespace-tolerant, because buildctl writes the metadata file
    # indented: "containerimage.digest": "sha256:..." has a space after the
    # colon and the first version of this pattern did not allow one. The build
    # had already run and pushed; only the reading failed, and the platform
    # reported "no digest" — which reads as a broken build rather than as a
    # broken sed.
    DIGEST=$(tr -d '\n' < "$META" \
      | sed -n 's/.*"containerimage\.digest"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    # And if it is still empty, hand over the file rather than a verdict about
    # it. A parser that cannot find what it wants knows less about why than the
    # bytes it was given.
    [ -n "$DIGEST" ] || fail "the build finished but no digest could be read from: $(head -c 400 "$META")"
    printf '{"digest":"%s","method":"%s"}' "$DIGEST" "$RAN" > "$RESULT"
    ;;
  buildpack)
    # Silence on purpose. The lifecycle container answers, and this one exiting
    # zero with an empty termination message is what lets it.
    ;;
esac
`

// buildpackScript turns a repository with no Dockerfile into an image.
//
// Cloud Native Buildpacks' lifecycle rather than the pack CLI: pack builds by
// driving a Docker daemon, and there is no daemon here and will not be one —
// mounting docker.sock into a pod is root on the node. The lifecycle binaries
// talk to a registry over HTTP instead, which is the only reason any of this
// fits in a cluster.
//
// The five phases are run one at a time rather than through the lifecycle's own
// `creator`, which bundles exactly these five into one process. creator is less
// code here and strictly worse to fail with: one exit status for five different
// jobs, so "the build failed" could mean the language was not recognised, or a
// dependency did not download, or the registry refused the push, and the
// control plane could not tell which. Run separately, the phase that failed is
// named by the thing that failed.
//
// The order is the specification's, not the obvious one. detect looks like it
// should come first and does not: the platform specification (Platform API 0.12
// through 0.15, all checked) mandates analyzer, detector, restorer, builder,
// exporter, because the analyzer resolves the run image and writes the
// analyzed.toml that the detector then updates. Running detect first appears to
// work and produces an image built against the wrong base.
//
// Every path below is read from the environment with the real default, so the
// script can be run for real against fake phases in a test. That is the whole
// reason the indirection exists: the rule this file is under is that a failure
// has to arrive carrying the builder's own words, and a rule nothing executes
// is decoration.
const buildpackScript = resultPrelude + `
need CACHE_IMAGE "${CACHE_IMAGE:-}"

PREPARED=${PREPARED:-` + preparedPath + `}
APP=${APP:-/workspace}
LAYERS=${LAYERS:-/layers}
LIFECYCLE=${LIFECYCLE:-/cnb/lifecycle}
ERR=${ERR:-/tmp/phase.err}
PLATFORM=${PLATFORM:-/tmp/platform}

[ -f "$PREPARED/method" ] || fail "the prepare step ended without saying how this repository should be built"
# Already built and already reported, by the container that made the decision.
# Exiting quietly leaves its answer as the only one there is.
[ "$(cat "$PREPARED/method")" = buildpack ] || exit 0

RAN=buildpack

# The build-time variables, in the shape the lifecycle reads them.
#
# Not an environment variable on this container, which is what the Dockerfile
# path uses: the CNB platform specification says a platform passes build-time
# variables as files under <platform>/env, one file per variable, the file's
# contents being the value. The lifecycle exports them into the build
# environment for the detector and the builder, which is where a buildpack
# looks. A variable left in this container's own environment reaches neither.
#
# printf rather than a redirect of printenv, because the file's contents ARE
# the value: redirecting printenv into the file writes a trailing newline, and
# the buildpack would receive a value with a line break at the end of it.
#
# The directory is made whether or not there is anything to put in it, so the
# -platform flag below is unconditional and this path has one shape rather than
# two.
mkdir -p "$PLATFORM/env" || fail "could not make the platform directory $PLATFORM/env"
for name in ${BUILD_ARGS:-}; do
  printf '%s' "$(printenv "BUILD_ARG_${name}" || true)" > "$PLATFORM/env/$name" \
    || fail "the build-time variable $name could not be written to $PLATFORM/env"
done

SRC="$PREPARED/src${PATH_IN_REPO:+/$PATH_IN_REPO}"
[ -d "$SRC" ] || fail "the path $PATH_IN_REPO does not exist in this repository"

# Copied, not built where it landed, and the mount is read-only to keep it that
# way. The clone belongs to the uid that cloned it, in a container that is not
# this one; the application directory in this image belongs to the buildpack
# user (1001:1000, measured from the image). Buildpacks write into the
# application directory, so handing them a tree they do not own fails somewhere
# inside somebody else's buildpack with a message about permissions.
mkdir -p "$APP"
cp -R "$SRC/." "$APP/" || fail "could not copy the checkout into $APP"

# The lifecycle's own stderr, forwarded rather than summarised.
#
# This is the rule this whole file is under: a build fails for reasons that
# belong to the user's code, and a platform that replaces the compiler's message
# with its own is a platform nobody can debug against. The phase name is put in
# front of the quote because it says which of the five spoke, and that is the
# one thing the quoted text never says.
#
# It also goes to the pod log. The termination message is capped and a real
# stack of buildpack output is not, so the short answer reaches the control
# plane and the long one stays somewhere it can still be read.
phase() {
  name=${1##*/}
  rc=0
  "$@" 2>"$ERR" || rc=$?
  if [ "$rc" != 0 ]; then
    cat "$ERR" >&2
    said=$(tail -c 700 "$ERR" 2>/dev/null || true)
    [ -n "$said" ] || said="the $name phase exited with status $rc and said nothing; see the job log"
    fail "$name: $said"
  fi
}

# Registry access to the previous image and the run image, and the analyzed.toml
# the rest of the phases build on.
phase "$LIFECYCLE/analyzer" -layers="$LAYERS" "$IMAGE"
# Which buildpacks want this repository. Exits 20 or 21 when none do, which is
# the honest answer to "we could not work out what this is".
phase "$LIFECYCLE/detector" -app="$APP" -layers="$LAYERS" -platform="$PLATFORM"
# Layers this build can reuse, from two different places. The previous image
# gives back the layers that ship in it; -cache-image gives back the ones that
# never do — the module cache, the compiler's own intermediate output — which is
# where a rebuild's time actually goes.
#
# A cache image rather than -cache-dir, for the reason written at cacheTag: the
# only disk this pod has dies with it, and the volume that would outlive it is
# ReadWriteOnce against a namespace that admits ten concurrent builds.
phase "$LIFECYCLE/restorer" -layers="$LAYERS" -cache-image="$CACHE_IMAGE"
# The user's code, compiled by somebody else's buildpack.
phase "$LIFECYCLE/builder" -app="$APP" -layers="$LAYERS" -platform="$PLATFORM"
# Assembles the layers onto the run image and pushes. No -process-type: the
# buildpacks name their own default, and naming one here that no buildpack
# provides fails the export.
# The same reference on the way out, or the restorer above has nothing to find.
# The exporter treats a cache it cannot write as a warning and still exports the
# image, which is the behaviour the Dockerfile path has to ask for by name with
# ignore-error.
phase "$LIFECYCLE/exporter" -app="$APP" -layers="$LAYERS" -cache-image="$CACHE_IMAGE" \
  -report="$LAYERS/report.toml" "$IMAGE"

# Whitespace-tolerant for the reason the Dockerfile path already learned: the
# file is indented TOML, digest = "sha256:..." carries spaces around the equals
# sign, and a pattern that had already pushed an image reported "no digest".
DIGEST=$(sed -n 's/^[[:space:]]*digest[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
  "$LAYERS/report.toml" 2>/dev/null | head -1)
[ -n "$DIGEST" ] || fail "the export finished but no digest could be read from: $(head -c 400 "$LAYERS/report.toml" 2>/dev/null || echo 'no report at all')"
printf '{"digest":"%s","method":"%s"}' "$DIGEST" "$RAN" > "$RESULT"
`

// How the build-time variables ride into the container.
//
// A list of names in one variable and one variable per value, rather than a
// single string the script would have to split. A value is user text: a space,
// a quote or a newline in it would be word-split on the way past, and the
// failure would be a build that used half a value without saying so.
//
// The two names cannot collide with each other whatever a user calls a
// variable, which is not an accident: the list is BUILD_ARGS and every value is
// BUILD_ARG_<name>, so a variable called NAMES becomes BUILD_ARG_NAMES and
// still is not the list. The pair BUILD_ARG_NAMES/BUILD_ARG_<name> was the
// first spelling and a variable called NAMES would have overwritten it.
//
// The prefix is also what keeps a user's variable away from the script's own.
// Unprefixed, a build-time variable called IMAGE, SRC or METHOD would replace
// the value this script runs on.
const (
	envBuildArgs        = "BUILD_ARGS"
	buildArgValuePrefix = "BUILD_ARG_"
)

// buildArgEnv turns the Build's variables into what both scripts read.
//
// The same variables reach both containers. Which of the two uses them is
// decided at run time by the method the prepare step wrote, and a container
// that carries variables it does not use costs nothing; a container that turns
// out to need them and does not have them costs a build.
func buildArgEnv(b *platformv1alpha1.Build) []corev1.EnvVar {
	if len(b.Spec.Env) == 0 {
		return nil
	}
	names := make([]string, 0, len(b.Spec.Env))
	out := make([]corev1.EnvVar, 0, len(b.Spec.Env)+1)
	for _, e := range b.Spec.Env {
		names = append(names, e.Name)
		out = append(out, corev1.EnvVar{Name: buildArgValuePrefix + e.Name, Value: e.Value})
	}
	// Space-separated, which is safe for exactly the reason the values are not:
	// the API refuses a name that is not a letter or underscore followed by
	// letters, digits and underscores, so a name can never contain a space.
	return append(out, corev1.EnvVar{Name: envBuildArgs, Value: strings.Join(names, " ")})
}

// desiredBuildJob renders the job that turns a commit into an image.
func desiredBuildJob(b *platformv1alpha1.Build) *batchv1.Job {
	labels := map[string]string{
		nameLabel:      b.Name,
		instanceLabel:  b.Name,
		componentLabel: buildComponent,
		managedByLabel: managedByDamga,
	}
	method := b.Spec.Builder
	if method == "" {
		method = platformv1alpha1.BuildDetect
	}

	res := b.Spec.Resources
	if res.MemoryLimit.IsZero() {
		// A build is the burstiest thing on the cluster and the only workload
		// here that is expected to end. Bounded so one repository's compile
		// cannot take the node away from every application running on it.
		res.MemoryLimit = resource.MustParse("2Gi")
	}
	if res.MemoryRequest.IsZero() {
		res.MemoryRequest = resource.MustParse("512Mi")
	}
	if res.CPURequest.IsZero() {
		res.CPURequest = resource.MustParse("250m")
	}
	limits := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    res.CPURequest,
			corev1.ResourceMemory: res.MemoryRequest,
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: res.MemoryLimit},
	}

	pod := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		},
		// Still no token. The concession below is about the node, not about
		// the API: a build has no business talking to the control plane, and it
		// reports its result through the termination log precisely so it never
		// needs to.
		AutomountServiceAccountToken: ptr.To(false),
		Volumes: []corev1.Volume{
			{Name: workspaceVolume, VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			}},
			{Name: tmpVolume, VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			}},
			{Name: "buildkit", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			}},
			{Name: runVolume, VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			}},
		},
	}

	// One container when the answer is already known, two when it is not.
	//
	// The pod's images are fixed when it is created and the kubelet pulls every
	// one of them whether the path that needs it runs or not. The builder image
	// is 1267 MB of compressed layers against buildkit's 101 MB, so a build
	// that was told to use a Dockerfile keeps exactly the shape it had before
	// buildpacks existed here — which is also the shape CI exercises against a
	// real cluster, and it is not made slower by a path it will not take.
	//
	// "detect" pays for the builder image even when it finds a Dockerfile. That
	// is the cost of the decision needing the working tree, and the alternative
	// — deciding in the controller, before the clone — is guessing about
	// somebody else's repository.
	if method == platformv1alpha1.BuildDockerfile {
		pod.Containers = []corev1.Container{buildkitSide(b, method, buildContainer, limits)}
	} else {
		pod.InitContainers = []corev1.Container{buildkitSide(b, method, prepareContainer, limits)}
		pod.Containers = []corev1.Container{lifecycleSide(b, limits)}
		// And only then, because only then is there an image that cannot run
		// everywhere. The builder tag resolves to a single linux/amd64 manifest
		// rather than an index (measured), so on an arm64 node — Apple silicon,
		// Graviton — the kubelet takes the pod, fails to run that image, and
		// with one attempt allowed the Job sits in the state this file already
		// has a comment about: a build that cannot start looks exactly like a
		// build that is slow.
		//
		// Asking the scheduler instead answers it twice. On a cluster that has
		// an amd64 node the build lands there and works; on one that has none
		// the pod stays Pending against an event that names the constraint.
		//
		// Temporary, and tied to the image rather than to the platform: it goes
		// when the builder is published as an index. buildkit's image already
		// is one, which is why a Dockerfile build is not pinned to anything.
		pod.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: b.Name, Namespace: b.Namespace, Labels: labels,
		},
		Spec: batchv1.JobSpec{
			// One attempt. A build fails because the code does not compile far
			// more often than because the cluster hiccuped, and retrying a
			// compile error three times turns a ten-second answer into a
			// thirty-second one while producing the same message.
			BackoffLimit: ptr.To(int32(0)),

			// And a wall clock, which BackoffLimit does not provide.
			//
			// Measured the hard way: a pod template that admission refuses is
			// not a pod failure, so it does not count against BackoffLimit at
			// all. The job controller retried the create twenty times in
			// fifteen minutes, the Job never reached a terminal condition, and
			// the Build sat in Running for ever — a build that cannot start
			// looked exactly like a build that is slow.
			//
			// activeDeadlineSeconds is measured from the Job's start rather
			// than from a pod's, so it bounds that case too.
			ActiveDeadlineSeconds: ptr.To(int64(30 * 60)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       pod,
			},
		},
	}
}

// buildkitSide clones, decides, and builds a Dockerfile.
func buildkitSide(
	b *platformv1alpha1.Build, method platformv1alpha1.BuildMethod,
	name string, limits corev1.ResourceRequirements,
) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   buildkitImage,
		Command: []string{"sh", "-c", buildScript},
		Env: append(buildArgEnv(b), []corev1.EnvVar{
			{Name: "REPO", Value: b.Spec.Repo},
			{Name: "REVISION", Value: b.Spec.Revision},
			{Name: envPathInRepo, Value: b.Spec.Path},
			{Name: envImage, Value: buildImageRef(b)},
			{Name: envCacheImage, Value: buildCacheRef(b)},
			{Name: "METHOD", Value: string(method)},
		}...),
		SecurityContext: &corev1.SecurityContext{
			// Privileged, and this is the concession the whole design turns on.
			// buildkitd creates mount and network namespaces for each build
			// step; rootless avoids needing that and is unavailable here.
			//
			// Written as one field rather than a list of capabilities on
			// purpose: CAP_SYS_ADMIN plus mount is most of privileged with a
			// longer spelling, and a spec that looks narrow while granting the
			// same thing is worse than one that says what it does. Narrowing it
			// is real work and is not done.
			//
			// Except when the build was told to use a buildpack, where this
			// container only clones. buildkitd never starts, so the privilege
			// is not a concession being made carefully — it is one not being
			// made at all.
			Privileged: ptr.To(method != platformv1alpha1.BuildBuildpack),
		},
		TerminationMessagePath:   BuildResultPath,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Resources:                limits,
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolume, MountPath: "/workspace"},
			{Name: tmpVolume, MountPath: tmpPath},
			{Name: "buildkit", MountPath: "/var/lib/buildkit"},
			{Name: runVolume, MountPath: "/run"},
		},
	}
}

// lifecycleSide runs the buildpack phases, or reports what the container before
// it already did.
func lifecycleSide(b *platformv1alpha1.Build, limits corev1.ResourceRequirements) corev1.Container {
	env := append(buildArgEnv(b), []corev1.EnvVar{
		{Name: envImage, Value: buildImageRef(b)},
		{Name: envCacheImage, Value: buildCacheRef(b)},
		{Name: envPathInRepo, Value: b.Spec.Path},
		{Name: "CNB_PLATFORM_API", Value: platformAPI},
		{Name: envHome, Value: buildpackHome},
	}...)
	if host := insecureRegistry(b.Spec.Image); host != "" {
		// The same concession the Dockerfile path already makes one line at a
		// time with registry.insecure=true, in the lifecycle's spelling. The
		// registry this platform installs speaks plain HTTP inside the cluster
		// and there is nowhere yet to put a credential for one that does not,
		// so ours is the only registry a build can currently push to. When that
		// changes both paths need the same answer, and neither should be the
		// one that quietly downgrades somebody's TLS.
		env = append(env, corev1.EnvVar{Name: "CNB_INSECURE_REGISTRIES", Value: host})
	}

	return corev1.Container{
		Name:    buildContainer,
		Image:   builderImage,
		Command: []string{"sh", "-c", buildpackScript},
		Env:     env,
		SecurityContext: &corev1.SecurityContext{
			// Neither privileged nor unconfined, and this is the difference
			// between the two containers rather than an oversight in one of
			// them. buildkitd needs the node; the lifecycle needs a filesystem
			// and a socket to the registry. This is also the container that
			// executes somebody else's buildpack against somebody else's code,
			// which is the last place to spend a privilege that is not needed.
			//
			// The seccomp profile is set here rather than on the pod, because
			// the pod-level one is Unconfined for buildkit's sake and a
			// container-level profile is what overrides it.
			Privileged:     ptr.To(false),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		TerminationMessagePath:   BuildResultPath,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Resources:                limits,
		VolumeMounts: []corev1.VolumeMount{
			// Read-only, so the copy into the application directory is the only
			// way the tree gets used and cannot be half-done in place.
			{Name: workspaceVolume, MountPath: preparedPath, ReadOnly: true},
		},
	}
}

// buildImageRef is where the result is pushed. The tag names the commit: a
// build whose output is called :latest cannot answer which commit is running.
func buildImageRef(b *platformv1alpha1.Build) string {
	return b.Spec.Image + ":" + b.Spec.Revision
}

// buildCacheRef is where the layers worth keeping are kept: the same repository
// as the images, under one tag shared by every revision of it. See cacheTag for
// why this is a registry reference and not a volume.
//
// Per repository and not per revision, which is the entire point — a cache
// tagged with the commit could only ever be read by a build of that same
// commit, which is a build that does not need one.
func buildCacheRef(b *platformv1alpha1.Build) string {
	return b.Spec.Image + ":" + cacheTag
}

// insecureRegistry is the registry host the lifecycle is told not to use TLS
// for, or empty when the reference names no host at all.
//
// Docker's own rule for telling a registry from the first path segment, because
// there is no other: a host has a dot or a port, or it is localhost. Without it
// "library/nginx" would name "library" as a registry and the push would go to
// Docker Hub over plain HTTP.
func insecureRegistry(image string) string {
	host, _, found := strings.Cut(image, "/")
	if !found {
		return ""
	}
	if host == "localhost" || strings.ContainsAny(host, ".:") {
		return host
	}
	return ""
}
