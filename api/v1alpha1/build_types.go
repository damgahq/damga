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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// BuildSpec is one commit turned into one image.
//
// A Build is a fact about the past, not a desired state — which makes it the
// odd one out in this API. Everything else here says what should be true and is
// reconciled towards for ever; a Build says "this commit was asked for" and is
// finished as soon as it succeeds or fails. It is a resource anyway, for two
// reasons: it keeps the write path unchanged (a build is requested by a commit,
// like everything else), and it makes every build an object somebody can look
// at afterwards rather than a log line.
//
// The whole spec is immutable. Rebuilding is a new Build, because a Build that
// can be edited cannot answer "what produced the image that is running".
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a build is a record of one commit; request another by creating a new Build"
type BuildSpec struct {
	// Repo is the git repository to build.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self.startsWith('https://') || self.startsWith('git@')",message="repo must be an https:// or git@ URL"
	// No credentials in the URL, and this rule is not hygiene.
	//
	// The prefix check above accepts
	// https://x-access-token:ghp_xxx@github.com/org/repo, git takes it as-is,
	// and a private repository builds today — through a door nobody opened on
	// purpose. Where the token then lives is the problem: in plain text on
	// Build.Spec.Repo, in a namespace holding every tenant's builds, in a field
	// the rule above makes immutable, on an object nothing deletes. A secret
	// that cannot be rotated or removed is worse than one that was never
	// accepted.
	//
	// Found by the session building the webhook, which checked a claim of mine
	// that the hole was only latent and showed it was not.
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('https://') || !self.substring(8).split('/')[0].contains('@')",message="the repository URL must not carry credentials; this field is immutable and a token written here cannot be removed"
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('git@') || !self.substring(4).split(':')[0].contains('@')",message="the repository URL must not carry credentials; this field is immutable and a token written here cannot be removed"
	Repo string `json:"repo"`

	// Revision is the commit to build.
	//
	// A full SHA and never a branch name. A branch is a moving target, and a
	// record that says "built main" cannot answer which main — which is the
	// only question anybody asks of it later.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Revision string `json:"revision"`

	// Path is the directory inside the repository to build, for a monorepo.
	// Empty is the root.
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/') && !self.contains('..')",message="path must be relative and must not climb out of the repository"
	Path string `json:"path,omitempty"`

	// Image is where the result is pushed, without a tag: the platform appends
	// the revision, so an image reference always names the commit that produced
	// it.
	//
	// The tag is looked for in the last path segment and not in the whole
	// string, because a registry carries a port: this rule first read
	// `!self.contains(':')` and refused `registry.damga-registry.svc:5000/ci/app`
	// — the platform's own registry, on the first build that ever ran. The
	// opposite half of the same trap is already written down in WorkloadSpec's
	// Image, which had to look in the last segment for the tag rather than at
	// the whole string. Same colon, both directions, and the note that existed
	// did not stop the second one.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="!self.split('/')[int(self.split('/').size()) - 1].contains(':')",message="image must not carry a tag; the revision is appended"
	Image string `json:"image"`

	// Builder chooses how the source becomes an image. Empty means detect:
	// a Dockerfile is used when there is one, and the language is detected when
	// there is not — which is what a user who has never written a Dockerfile
	// expects to happen.
	// +kubebuilder:validation:Enum=detect;dockerfile;buildpack
	// +kubebuilder:default=detect
	Builder BuildMethod `json:"builder,omitempty"`

	// Env are build-time environment variables: values that choose what is
	// compiled, not what the running container talks to.
	//
	// The two are separate lists on a Workload for the reason Coolify's
	// is_buildtime and is_runtime are two booleans and not one: they have
	// different lifetimes, and delivering either to the other is a real
	// mistake — an API key baked into an image layer, or a compiler flag in a
	// running container's environment. What reaches a Build is the build-time
	// half of an app's settings, carried here by whoever asked for the build.
	//
	// Literal values only, and a build-time secret is refused rather than
	// half-supported. Builds run in their own namespace; a Secret in the
	// tenant's is not reachable from there, so the value would have to be
	// copied into the build namespace by something that can read it, and
	// nothing in this platform can read a secret. That is deliberate and it is
	// what makes the settings endpoint's promise structural. A build-time
	// secret is a separate change with a separate answer.
	//
	// Everything in this list is therefore as readable as the repository the
	// manifest lives in — and as readable as this object, which shares a
	// namespace with every other tenant's builds.
	//
	// A name envFrom would deliver, for the same reason the workload's user
	// secrets are checked: a name that is not a legal environment variable is
	// one the build silently does not receive.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:XValidation:rule="self.all(e, e.name.matches('^[A-Za-z_][A-Za-z0-9_]*$'))",message="a build-time variable needs a name a shell can carry: a letter or underscore, then letters, digits and underscores"
	Env []EnvVar `json:"env,omitempty"`

	Resources Resources `json:"resources,omitempty"`
}

// The labels every Build carries, named here because two packages read them and
// a label key spelled apart in two places is a lookup that silently finds
// nothing.
//
// The control plane sets them when it creates the Build — a tenant id cannot be
// part of the name, since names are DNS labels and tenant ids are not — and the
// operator groups by them when it decides which records to keep. They are the
// only way to answer "whose build is this" from the cluster, because every
// tenant's builds share one namespace.
const (
	BuildTenantLabel   = "damga.co/tenant"
	BuildAppLabel      = "damga.co/app"
	BuildRevisionLabel = "damga.co/revision"
)

// BuildMethod is how source becomes an image.
type BuildMethod string

const (
	// BuildDetect uses the Dockerfile if the repository has one and detects the
	// language if it does not.
	BuildDetect BuildMethod = "detect"
	// BuildDockerfile always uses the Dockerfile, and fails when there is none
	// rather than quietly building something else.
	BuildDockerfile BuildMethod = "dockerfile"
	// BuildBuildpack always detects the language, ignoring any Dockerfile.
	BuildBuildpack BuildMethod = "buildpack"
)

// BuildPhase is where a build got to.
type BuildPhase string

const (
	BuildPending   BuildPhase = "Pending"
	BuildRunning   BuildPhase = "Running"
	BuildSucceeded BuildPhase = "Succeeded"
	BuildFailed    BuildPhase = "Failed"
)

// BuildStatus is what happened, reported and never inferred.
type BuildStatus struct {
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	Phase BuildPhase `json:"phase,omitempty"`

	// Digest is the image the build produced, and it is the point of the whole
	// resource: a tag can be moved and a digest cannot, so this is what gets
	// written into the Workload.
	//
	// Reported by the build itself through its termination log, which is the
	// only channel a job with no API token has. Giving a build container a
	// cluster identity would hand an identity to whatever it is compiling.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`

	// Method is what the build actually did, which is not always what was
	// asked: "detect" resolves to one of the others, and knowing which one ran
	// is the difference between a reproducible record and a guess.
	Method BuildMethod `json:"method,omitempty"`

	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Message is why a failed build failed, quoted from the builder rather than
	// summarised. A build fails for reasons that belong to the user's code, and
	// a platform that paraphrases the compiler is a platform nobody can debug
	// against.
	Message string `json:"message,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Method",type=string,JSONPath=`.status.method`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.spec.revision`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Build turns one commit into one image.
type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BuildSpec   `json:"spec,omitempty"`
	Status BuildStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BuildList contains a list of Build.
type BuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Build `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Build{}, &BuildList{})
		return nil
	})
}
