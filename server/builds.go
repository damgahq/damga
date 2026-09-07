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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/authz"
)

const (
	// BuildNamespace is where every Build this platform starts is created.
	//
	// One namespace for all of them and never beside the app being built.
	// cluster/build-namespace.yaml carries the measurement that forced it:
	// rootless BuildKit does not start on the hosts this targets, so builds run
	// privileged, and the containment is therefore the namespace rather than the
	// pod. That namespace is the only privileged one this platform creates, it
	// carries a quota so a queue of builds cannot take the machine, and its
	// NetworkPolicy lets a build reach the internet and the registry and nothing
	// else in the cluster.
	//
	// A constant here and a YAML file over there, which is a duplication worth
	// naming: the file is what creates the namespace and this is what puts
	// objects in it. They have to agree, and nothing checks that they do.
	BuildNamespace = "damga-build"

	// defaultRegistry is what Config.Registry falls back to: the platform's own
	// in-cluster registry, from cluster/registry.yaml. The port is fixed rather
	// than allocated because containerd's redirect names it in a file written
	// when the node is created.
	//
	// The one literal, read by BindFlags for the flag's default and by
	// withDefaults for an Options nobody passed a flag to. Both readers name
	// this identifier rather than repeating the string, because a value written
	// out twice is a value that disagrees with itself later — this repository
	// lost two CI rounds to buildHome being spelled in three places, and had an
	// operator reconcile a type it had never heard of because a kustomization
	// list was maintained by hand.
	//
	// The deployed value does not come from here at all. cluster/control-plane.yaml
	// passes -registry, so what a real install pushes to is read from the Service
	// in cluster/registry.yaml rather than from this file.
	defaultRegistry = "registry.damga-registry.svc:5000"
)

// revisionPattern is the same rule the CRD enforces: a full commit SHA and
// never a branch name.
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// BuildCreator hands a Build to the cluster.
//
// A seam for the reason BackupReader is one, plus a sharper one of its own:
// this is the only thing in this server that would WRITE to the cluster.
// Everything else writes to git and reads from a database, and Principle 1 says
// that is the whole write path. A build is the documented exception — it stands
// in front of the write path rather than inside it, producing a digest that a
// later commit carries — so the exception is a named interface with one method
// rather than a client handed around.
//
// It is nil in this build, and that is not an oversight; see createBuild.
type BuildCreator interface {
	// CreateBuild creates the Build and fills in what the API server assigned,
	// which for a generated name is the name itself.
	CreateBuild(ctx context.Context, build *platformv1alpha1.Build) error
}

// A quota refusal arrives as Forbidden, which is what every permission failure
// also arrives as — so until these two existed, a full namespace was reported
// as "the control plane is not permitted to create builds", with 501. That
// sentence is true of a missing RoleBinding and sends whoever reads it to the
// chart, while the actual cause is a namespace with no room in it. It is the
// same shape this repository has been bitten by twice: one sentence that is
// true for several causes.
//
// The two are told apart by the word the API server uses. A quota refusal names
// the quota; an authorization refusal names a user, a verb and a resource and
// never the word quota. Read from the admission plugin's message rather than
// measured here, because a quota is only enforced where a controller computes
// its usage and envtest runs none: measured, five creates against a quota of
// two all succeeded. The CI step "the build quota refuses out loud" is where
// this is checked against a real one.
func isQuotaExhausted(err error) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "exceeded quota")
}

// isQuotaUncounted is the other half, and it is not fullness at all: a quota
// that names a resource the API server has never heard of cannot compute a
// usage for it, and refuses every create until it can. This repository has the
// message written down from the run that produced it — see
// cluster/build-namespace.yaml — which is why it is matched exactly.
func isQuotaUncounted(err error) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "status unknown for quota")
}

// createBuildRequest is one commit to turn into one image.
//
// It names a repository because nothing else in the platform knows one. A
// placement's repository is the tenant's STATE repository — where damga commits
// manifests — and the source a build clones is a different repository that no
// row records: the endpoints that used to connect one were deleted along with
// the signature chain. Until something records it, the caller carries it.
type createBuildRequest struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	Path     string `json:"path,omitempty"`
	Builder  string `json:"builder,omitempty"`

	// Image is where the result is pushed, without a tag. Empty means the
	// platform's own registry under this tenant and app.
	Image string `json:"image,omitempty"`
}

// createBuild asks the cluster to turn one commit into one image.
//
// # This endpoint cannot work in this build, on purpose
//
// Creating a Build needs create on builds.platform.damga.co in damga-build, and
// this control plane has no such right. The obvious reading is that a Role is
// missing from chart/, and that is not the whole of it: there is no
// ServiceAccount to grant it to, because there is no control plane running in
// the cluster to hold one. Checked rather than assumed — the repository has a
// Dockerfile.operator and no Dockerfile for cmd/damga, chart/values.yaml
// deploys `damga-app`, which CI builds with `context: ./app` (the demo), and
// docs/CONTROL-PLANE.md tells the reader to run the server as a binary on their
// machine.
//
// What this server does hold is read access, through clusterReader in
// manager.go, and that is out-of-cluster too: it exists so one page can show a
// database's last rehearsal. So the seam is left nil and the endpoint answers
// 501.
//
// Answering 501 rather than pretending is the same decision backupRoute makes
// when it has no cluster reader, and for the same reason: the empty answer and
// the missing capability read identically to whoever is looking at the page,
// and only one of them is true. "This installation cannot start builds" and
// "the build could not be started" send a reader to two different places, and
// the first one is where the fix is.
//
// Everything up to the write is real and tested. The request is parsed, every
// rule the CRD will enforce is checked here first so that a refusal is a
// sentence rather than a CEL expression, and the Build that would be created is
// built. What is missing is one wiring line and one Role.
func createBuild(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// app:deploy. A build is the first half of a deploy — it exists to
		// produce the digest a deploy commits — so the right to start one is
		// the right to ship one. Splitting them would let somebody spend the
		// cluster's build quota without being able to use the result.
		_, ref, ok := g.admit(w, r, authz.ActionAppDeploy)
		if !ok {
			return
		}

		var req createBuildRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "the request body is not the expected JSON")
			return
		}
		build, err := buildFor(st.registry, ref.TenantID, ref.App, req)
		if err != nil {
			problem(w, http.StatusBadRequest, err.Error())
			return
		}

		if st.builds == nil {
			problem(w, http.StatusNotImplemented,
				"this installation cannot start builds: the control plane has no permission to create them in "+
					BuildNamespace)
			return
		}
		switch err := st.builds.CreateBuild(r.Context(), build); {
		// No cluster to build in. Kept apart from every refusal below, which
		// are all things a cluster said: this one is the installation not
		// having one, and it is the same 501 the nil seam used to give — the
		// seam is no longer nil because it is now resolved lazily, so the case
		// moved here rather than disappearing.
		case errors.Is(err, errBuildsUnavailable):
			problem(w, http.StatusNotImplemented,
				"this installation cannot start builds: "+err.Error())
			return
		case isQuotaUncounted(err):
			// The quota counts a type the API server has not been told about.
			// Its own words name the quota and not the missing type, which is
			// why this sentence exists: this repository lost a CI round to it
			// once, and the fix is an ordering — the Build CRD goes in before
			// the quota that counts it. See cluster/build-namespace.yaml.
			problem(w, http.StatusInternalServerError,
				"this installation's build quota cannot count Build objects: the Build CRD has "+
					"to be installed before the quota that counts it. The API server said: "+
					err.Error())
			return
		case isQuotaExhausted(err):
			// Full, which is a different thing from forbidden and used to be
			// reported as it. 503 rather than 500: nothing is broken, there is
			// no room, and the operator's collector frees some as builds finish.
			problem(w, http.StatusServiceUnavailable,
				"this installation has no room for another build record in "+BuildNamespace+
					": the namespace quota is full and the records are collected as builds "+
					"finish. The API server said: "+err.Error())
			return
		case apierrors.IsForbidden(err):
			// The wiring exists and the permission does not. Named apart from
			// every other cluster failure because it is the one an operator
			// fixes in the chart rather than by retrying.
			problem(w, http.StatusNotImplemented,
				"the control plane is not permitted to create builds in "+BuildNamespace)
			return
		case err != nil:
			problem(w, http.StatusBadGateway, "the build could not be started: "+err.Error())
			return
		}

		// Accepted, not created. A Build is a request that a job will answer,
		// and the digest — the only part anybody wants — arrives minutes later
		// through the build's own status.
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"build": map[string]string{
			"name":      build.Name,
			"namespace": build.Namespace,
			"image":     build.Spec.Image,
			"revision":  build.Spec.Revision,
			"builder":   string(build.Spec.Builder),
		}})
	})
}

// buildFor turns a request into the object that would be created.
//
// A function of its own, and every rule in it is a copy of one the CRD already
// enforces. The duplication is deliberate and has a boundary: the CRD is the
// enforcement — it is what refuses an object created by kubectl, by a future
// CLI, or by this server after somebody edits it — and this is only the
// message. A caller who sends a branch name where a commit belongs gets "a
// build needs a full 40-character commit sha" instead of a CEL rule quoted back
// at them. If the two ever drift, the CRD still refuses; what is lost is the
// sentence, not the guarantee.
// credentialInURL reports whether a repository URL carries userinfo.
//
// The authority is everything before the first path separator: for https that
// is up to "/", for git@host:path it is up to ":". A "@" there is a credential.
// git@ is expected to have exactly one, which is why the search starts after it.
func credentialInURL(repo string) bool {
	switch {
	case strings.HasPrefix(repo, "https://"):
		authority, _, _ := strings.Cut(strings.TrimPrefix(repo, "https://"), "/")
		return strings.Contains(authority, "@")
	case strings.HasPrefix(repo, "git@"):
		authority, _, _ := strings.Cut(strings.TrimPrefix(repo, "git@"), ":")
		return strings.Contains(authority, "@")
	}
	return false
}

func buildFor(registry, tenantID, app string, req createBuildRequest) (*platformv1alpha1.Build, error) {
	repo := strings.TrimSpace(req.Repo)
	revision := strings.TrimSpace(req.Revision)
	path := strings.TrimSpace(req.Path)
	image := strings.TrimSpace(req.Image)

	switch {
	case repo == "":
		return nil, fmt.Errorf("a build needs the repository to clone")
	case !strings.HasPrefix(repo, "https://") && !strings.HasPrefix(repo, "git@"):
		return nil, fmt.Errorf("the repository must be an https:// or git@ URL")
	// Refused here as well as by the API server, so the caller is told at the
	// endpoint rather than by a CEL message. Same reason as the rule there: the
	// URL is the one place a credential can enter this field, the field is
	// immutable, and nothing deletes the object it lands on.
	case credentialInURL(repo):
		return nil, fmt.Errorf(
			"the repository URL must not carry credentials — this is recorded permanently " +
				"and cannot be removed; use a public repository, or a deploy key on the platform")
	case !revisionPattern.MatchString(revision):
		// Not a branch, and this is the rule most likely to be hit. A record
		// that says "built main" cannot answer which main, which is the only
		// question anybody asks of a build afterwards.
		return nil, fmt.Errorf("a build needs a full 40-character commit sha, lowercase")
	case strings.HasPrefix(path, "/") || strings.Contains(path, ".."):
		return nil, fmt.Errorf("the path must be relative and must not climb out of the repository")
	}

	method := platformv1alpha1.BuildMethod(strings.TrimSpace(req.Builder))
	switch method {
	case "":
		// Set explicitly rather than left for the CRD's default to fill in, so
		// that the object this function returns is the object that runs — which
		// is what makes it worth testing.
		method = platformv1alpha1.BuildDetect
	case platformv1alpha1.BuildDetect, platformv1alpha1.BuildDockerfile, platformv1alpha1.BuildBuildpack:
	default:
		return nil, fmt.Errorf("the builder must be detect, dockerfile or buildpack")
	}

	if image == "" {
		// The tenant id and not its slug: a slug can be renamed and a pushed
		// image cannot be, so a repository named after one would say something
		// that stopped being true. Underscores are legal in a registry path
		// component, which is why the id can go in unaltered where it cannot go
		// into an object name.
		//
		// The registry is passed in rather than read from the constant, so that
		// this function has no opinion about which install it is running in and
		// an install whose registry is elsewhere does not have to name an image
		// on every request.
		image = registry + "/" + tenantID + "/" + app
	}
	// The CRD's own rule, and the trap it records: the tag is looked for in the
	// last path segment and not in the whole string, because a registry carries
	// a port and `registry.damga-registry.svc:5000/ci/app` has a colon in it.
	if segs := strings.Split(image, "/"); strings.Contains(segs[len(segs)-1], ":") {
		return nil, fmt.Errorf("the image must not carry a tag; the revision is appended")
	}

	return &platformv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: BuildNamespace,
			// Generated and not deterministic, because rebuilding the same
			// commit is a new Build — the spec is immutable, so it is the only
			// way to ask again. A name derived from the commit would make the
			// second attempt a 409 on an object nobody can edit.
			//
			// The prefix carries no tenant id: every Build of every tenant
			// lives in one namespace, so the name has to be unique across them,
			// and a generated suffix gives that where a tenant id could not
			// anyway — an id with an underscore in it is not a legal name.
			GenerateName: app + "-" + revision[:12] + "-",
			Labels: map[string]string{
				// Which tenant asked, on the object rather than only in a log.
				// Builds outlive the request that made them and share a
				// namespace with every other tenant's, so this is the only way
				// to answer "whose build is this" from the cluster.
				platformv1alpha1.BuildTenantLabel:   tenantID,
				platformv1alpha1.BuildAppLabel:      app,
				platformv1alpha1.BuildRevisionLabel: revision,
				"app.kubernetes.io/managed-by":      managedByDamga,
			},
		},
		Spec: platformv1alpha1.BuildSpec{
			Repo: repo, Revision: revision, Path: path,
			Image: image, Builder: method,
		},
	}, nil
}
