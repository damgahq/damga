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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EnvVar is a literal environment variable. Anything that should not be read
// from `kubectl get workload -o yaml` belongs in a Secret listed under
// EnvFrom instead.
type EnvVar struct {
	// +kubebuilder:validation:MinLength=1
	// A ceiling, and it was put here by a measurement rather than by taste. A
	// CEL rule that checks the names in a list costs the API server an estimate
	// based on the longest string the schema allows, and with no maximum at all
	// the rule on BuildSpec.Env was refused for exceeding the budget by 12.1x —
	// taking the whole Build CRD with it, including rules that had been there
	// for releases. 128 is far more than any environment variable name anybody
	// writes and enough for the estimate to fit.
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	Value string `json:"value,omitempty"`
}

// Resources is what the container asks for and what it may not exceed. Both are
// required: a pod with no request is scheduled blind and a pod with no memory
// limit can take a node down with it. The admission policy rejects workloads
// that omit either, so there is no point in letting a Workload omit them.
//
// Raising only the request past the default limit is an easy mistake to make and
// produces a Deployment the API server refuses, so it is caught at admission
// where the message can name the field.
// +kubebuilder:validation:XValidation:rule="quantity(self.memoryLimit).compareTo(quantity(self.memoryRequest)) >= 0",message="memoryLimit must be greater than or equal to memoryRequest"
type Resources struct {
	// +kubebuilder:default="100m"
	CPURequest resource.Quantity `json:"cpuRequest,omitempty"`

	// +kubebuilder:default="128Mi"
	MemoryRequest resource.Quantity `json:"memoryRequest,omitempty"`

	// There is deliberately no CPU limit. CPU is compressible — throttling a
	// container is better than the latency cliff a limit produces — while memory
	// is not, so exceeding it has to mean eviction.
	// +kubebuilder:default="512Mi"
	MemoryLimit resource.Quantity `json:"memoryLimit,omitempty"`
}

// Health is how the platform decides whether a replica should receive traffic
// and whether it is still alive. The two questions are answered separately on
// purpose: a readiness failure removes one replica from a Service, a liveness
// failure restarts it. Pointing liveness at a dependency turns one slow database
// into every replica restarting at once.
// +kubebuilder:validation:XValidation:rule="!has(self.timeoutSeconds) || !has(self.intervalSeconds) || self.timeoutSeconds <= self.intervalSeconds",message="the probe timeout must not exceed the interval between probes"
type Health struct {
	// +kubebuilder:default="/healthz"
	LivenessPath string `json:"livenessPath,omitempty"`

	// +kubebuilder:default="/readyz"
	ReadinessPath string `json:"readinessPath,omitempty"`

	// Port the probes call. Zero means spec.port, which is what almost every
	// workload wants.
	//
	// It is here because the assumption underneath it is wrong often enough to
	// be the single most common way a catalogue install fails to become Ready:
	// an application that serves its API on one port and its health endpoint on
	// another gets probed on the wrong one, answers nothing, and is restarted
	// for ever by a liveness probe that was never pointed at it. Coolify has
	// this field; this had a comment saying the port was assumed.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// IntervalSeconds is how often the probes run. Zero keeps the platform's
	// five, which is right for a web service and far too impatient for
	// something that takes a second to answer under load.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// TimeoutSeconds is how long one probe may take. Zero keeps three.
	//
	// It has to be no larger than the interval: a probe that is still running
	// when the next one is due is a probe whose period is not the period.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is how many failures in a row mean unhealthy. Zero keeps
	// three. Kubernetes' own name for it; a panel is free to call it retries.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// Autoscale replaces a fixed replica count with a range. Scale-up is immediate
// and scale-down is deliberately slow: being late to scale up costs users,
// being hasty to scale down causes thrashing.
//
// The bound is checked here rather than left to the HorizontalPodAutoscaler.
// Without it the CRD accepts max < min, the HPA is then rejected as invalid, and
// reconciliation fails on every pass with an error the author never sees.
// +kubebuilder:validation:XValidation:rule="self.maxReplicas >= self.minReplicas",message="maxReplicas must be greater than or equal to minReplicas"
type Autoscale struct {
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:default=2
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=60
	TargetCPUPercent int32 `json:"targetCPUPercent,omitempty"`
}

// WorkloadSpec is the whole surface a user writes. Everything absent from it
// is a decision the platform has already made: non-root, read-only root
// filesystem, all capabilities dropped, no service-account token, default-deny
// networking, a disruption budget, and a rollout that never takes a replica out
// before its replacement is ready. Those are not defaults — there is no field
// that turns them off.
// +kubebuilder:validation:XValidation:rule="has(self.replicas) ? !has(self.autoscale) : true",message="set replicas or autoscale, not both"
// The claims a volume produces are ReadWriteOnce, which one node may mount at a
// time. A second replica scheduled elsewhere stays Pending for ever with
// "Multi-Attach error" while the first keeps serving — so the symptom is not an
// outage but an autoscaler that silently never scales. Refused here rather than
// in the controller, so a manifest applied with kubectl is refused too.
// +kubebuilder:validation:XValidation:rule="!(has(self.autoscale) && has(self.volumes) && self.volumes.size() > 0)",message="a workload with volumes cannot autoscale: its storage is ReadWriteOnce and only one node can mount it"
type WorkloadSpec struct {
	// Image is the container image to run. A digest is preferred; a tag is
	// accepted as long as it is not :latest, which means "whatever is there
	// today" and makes a rollback restore something other than what was rolled
	// back from.
	// +kubebuilder:validation:MinLength=1
	// The tag has to be looked for in the last path segment. Checking the whole
	// string lets `registry.local:5000/team-a/app` through — the colon belongs to
	// the registry port, there is no tag at all, and the kubelet resolves that to
	// :latest. The rule would then admit precisely what it exists to forbid.
	// +kubebuilder:validation:XValidation:rule="self.contains('@') || self.split('/')[int(self.split('/').size()) - 1].contains(':')",message="image must carry an explicit tag or digest"
	// +kubebuilder:validation:XValidation:rule="!self.endsWith(':latest')",message="image must not use the :latest tag"
	Image string `json:"image"`

	// Port is the port the container listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	Port int32 `json:"port,omitempty"`

	// Replicas is a fixed replica count. Leave it unset and set autoscale
	// instead to hand the decision to the HorizontalPodAutoscaler.
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Domain publishes the workload at a hostname over HTTPS. Without it the
	// workload is reachable only inside the cluster.
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$')",message="domain must be a lowercase DNS name"
	Domain string `json:"domain,omitempty"`

	// Env holds literal environment variables. These are visible to anyone who
	// can read the Workload, so credentials belong in EnvFrom.
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// EnvFrom names Secrets whose keys are injected as environment variables.
	// The Secret is referenced, never copied, so its value never appears in this
	// object or in whatever produced it.
	// +listType=set
	EnvFrom []string `json:"envFrom,omitempty"`

	// Database names a Database in this namespace whose credentials this
	// workload should receive.
	//
	// A name and nothing else. There is no owner reference in either direction,
	// which is the whole point of the two being separate kinds: an app is
	// redeployed many times a day and its data outlives every one of those
	// deploys, so deleting the app must not take the database with it.
	//
	// What arrives is the Secret the Database publishes, injected the way
	// EnvFrom injects any other — host, port, user, password and database name.
	// Referenced and never copied, so no credential appears in this object or in
	// the git commit that produced it.
	//
	// Same namespace, deliberately. A Secret readable from another namespace is
	// a tenant boundary with a hole in it, and the namespace-per-tenant
	// arrangement is what makes this reference safe without a second check.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Database string `json:"database,omitempty"`

	// RestartedAt cycles the pods without changing anything about what runs.
	//
	// A field rather than a reused annotation, and the difference was found by
	// a test rather than reasoned out: the deployment controller hashes
	// .spec.template alone, so anything already on the template that changes
	// per deploy — the rollout id, for instance — would roll every pod twice on
	// every deploy. This changes when a restart is asked for and at no other
	// time, which is what makes it safe to put where it has to go.
	//
	// Free text rather than a timestamp type, because the platform writes it
	// and nothing reads it: its only job is to differ from the last one.
	// +kubebuilder:validation:MaxLength=64
	RestartedAt string `json:"restartedAt,omitempty"`

	// Secrets are values the platform has to invent, and the workload needs.
	//
	// A request, not a value. The name of the environment variable and what kind
	// of thing to make; never the thing itself, which is the whole point — a
	// value written here would be a credential in the object, in the git commit
	// that produced it, and in every diff of both.
	//
	// The operator mints them and reads back what it already minted, exactly the
	// way the Database's own password works (desiredDatabaseSecret). That
	// precedent is why this is here rather than in the control plane: a control
	// plane that writes the Secret directly puts a value in the cluster that git
	// has never seen, so an install rebuilt from git alone comes back without
	// it — and silently, because nothing is missing from the manifest.
	//
	// Catalogue templates are what forced it: measured against the 341 that
	// convert, 159 ask for a generated value, and refusing all of them leaves
	// less than half the catalogue installable.
	//
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Secrets []GeneratedSecret `json:"secrets,omitempty"`

	// Volumes are directories that survive the pod.
	//
	// Needed by most of what anybody actually installs: measured against the 371
	// service templates this project converts from, 345 of them mount at least
	// one. Without this field a catalogue is twenty applications, not a
	// catalogue.
	//
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=8
	Volumes []Volume `json:"volumes,omitempty"`

	// BuildEnv are variables for the build that produces this app's image, and
	// not for the container that runs it.
	//
	// Two lists rather than one list with a flag, and the flag is the design
	// Coolify got right: a variable can be build-time, run-time, both or
	// neither, because the two have different lifetimes. A build-time variable
	// chooses what is compiled in; a run-time one chooses what the process
	// talks to. Delivering either to the other is a real mistake — an API key
	// baked into an image layer, or a compiler flag in a running container's
	// environment.
	//
	// Nothing reads this yet. BuildSpec has no environment at all, so a build
	// cannot receive one, and the endpoint that writes this says so in the
	// answer rather than letting somebody set a value that goes nowhere. The
	// list is here because the setting has to survive between being typed and
	// being consumed, and git is where this platform keeps what it was told.
	//
	// Literal values only. A build-time secret is refused, and not for lack of
	// a field: builds run in their own namespace and a Secret in the tenant's
	// is not reachable from it, so the value would have to be copied there by
	// something that can read it. Nothing in this platform can, deliberately.
	//
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	BuildEnv []EnvVar `json:"buildEnv,omitempty"`

	// UserSecrets are the names of environment variables whose values this
	// platform holds in a Secret instead of in git.
	//
	// The counterpart of Secrets above, and the missing third case. There were
	// two: a literal in Env, which is in the commit and readable by anyone who
	// can read the repository, and a value the platform invents, which never
	// exists outside the cluster. The one nobody could express was a value the
	// *user* has — a database address, an API key, the token an n8n template
	// asks for — and without it a catalogue entry that needs one cannot be
	// installed at all.
	//
	// Names and never values, which is the whole point. The value is written
	// straight into the Secret this platform keeps beside the workload
	// (<name>-env) and never appears here, in the commit, or in any diff of
	// either. What git carries is that the variable exists and where it lives,
	// which is more than a platform holding the value in its own database can
	// show.
	//
	// The cost is written down in docs/KARARLAR.md and it is real: a value that
	// is not in git has no rollback, and nothing restores it if the Secret is
	// deleted. A manifest comes back in about ten seconds; this does not come
	// back at all.
	//
	// A valid environment variable name, because envFrom silently skips a key
	// that is not one — the pod starts, the variable is absent, and the only
	// trace is an event nobody is reading.
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:items:MaxLength=128
	UserSecrets []string `json:"userSecrets,omitempty"`

	Resources Resources `json:"resources,omitempty"`

	Health Health `json:"health,omitempty"`

	// Autoscale hands the replica count to a HorizontalPodAutoscaler.
	//
	// Refused together with volumes; see the rule on WorkloadSpec.
	Autoscale *Autoscale `json:"autoscale,omitempty"`
}

// GeneratedSecret is one value the platform invents once and then keeps.
type GeneratedSecret struct {
	// Name is the environment variable the workload reads it from.
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Z0-9_]*$`
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`

	// Kind is what to make.
	//
	// Not a length or a character set, because those are decisions the platform
	// should own and change: a template that asked for "16 characters" would
	// pin this API to whatever was thought adequate the day it was written.
	// +kubebuilder:validation:Enum=password;hex;base64;user
	// +kubebuilder:default=password
	Kind GeneratedKind `json:"kind,omitempty"`
}

// GeneratedKind is the shape of a generated value. The names come from the
// compose templates this converts from, where they are spelled
// SERVICE_PASSWORD_X, SERVICE_HEX_X and SERVICE_BASE64_X.
type GeneratedKind string

const (
	// GeneratedPassword is a URL-safe random string. What almost everything
	// wants, and what a database password already is here.
	GeneratedPassword GeneratedKind = "password"
	// GeneratedHex is lowercase hex, for things that parse their secret as one
	// — a signing key expecting 64 hex characters will not accept base64.
	GeneratedHex GeneratedKind = "hex"
	// GeneratedBase64 is standard base64, for the same reason in the other
	// direction.
	GeneratedBase64 GeneratedKind = "base64"
	// GeneratedUser is an account name: short, lowercase, starting with a
	// letter, and safe everywhere a username is accepted — a PostgreSQL role,
	// a MySQL grant, a basic-auth pair.
	//
	// A kind of its own rather than a shorter password, which is the whole
	// reason it is here: a value out of a password generator is not something a
	// database will accept as a role, and it is not something a person reads
	// back off a screen. Measured before adding — of 341 catalogue templates,
	// 61 are unreachable for the want of exactly this, more than the next three
	// limits combined.
	GeneratedUser GeneratedKind = "user"
)

// Volume is one directory that outlives the pod.
type Volume struct {
	// Name identifies the claim within the workload. It becomes part of the
	// PersistentVolumeClaim's name, so it is a DNS label rather than free text.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Name string `json:"name"`

	// Path is where the volume is mounted inside the container.
	//
	// Absolute, and not /tmp: the renderer already mounts an emptyDir there so a
	// read-only root filesystem has somewhere to write, and a second volume at
	// the same path would shadow it.
	// +kubebuilder:validation:Pattern=`^/.+`
	// +kubebuilder:validation:XValidation:rule="self != '/tmp' && !self.startsWith('/tmp/')",message="/tmp is already an emptyDir; mount persistent storage elsewhere"
	Path string `json:"path"`

	// Size is how much storage to request.
	//
	// Required, with no default. A default here would be a number nobody chose
	// silently becoming the size of somebody's data directory — and a claim
	// cannot be shrunk afterwards.
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="size must be greater than zero"
	Size resource.Quantity `json:"size"`

	// StorageClass names the class to provision from. Empty means the cluster's
	// default class, which is what a single-node install has.
	// +kubebuilder:validation:MaxLength=253
	StorageClass string `json:"storageClass,omitempty"`
}

// WorkloadStatus reports what the platform observed, never what it was asked
// for. A field here that merely echoes the spec would make an Workload look
// healthy before anything had happened.
type WorkloadStatus struct {
	// Conditions follows the standard Kubernetes convention. Two are reported:
	//
	//   Ready  whether the workload is serving.
	//   TLS    whether a certificate has been issued for spec.domain. Present
	//          only while there is a domain, and removed with it.
	//
	// The two are deliberately independent rather than one condition. A
	// workload whose certificate is still pending is serving — its pods are up
	// and the ingress routes to them, over the ingress controller's own
	// certificate — so folding TLS into Ready would report it the same way as a
	// workload that cannot start at all.
	//
	// There is no Progressing condition. This said there was one for as long as
	// the field has existed and nothing ever wrote it.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// URL is where the workload answers, once an ingress exists for it. It is
	// https:// from the moment a domain is set, which is what the address will
	// be — whether a browser accepts it yet is the TLS condition's answer, not
	// this field's.
	URL string `json:"url,omitempty"`

	Replicas int32 `json:"replicas,omitempty"`

	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedGeneration is the spec generation this status describes. When it
	// trails metadata.generation the status is about the previous spec, which is
	// the difference between "not ready yet" and "ready, but not to this".
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// There is deliberately no scale subresource. It would have to point at
// .spec.replicas, which is absent whenever autoscaling owns the count, and
// `kubectl scale` against a missing path returns a 500. Scaling belongs to the
// autoscaler or to the replica count in the spec.
// +kubebuilder:resource:shortName=wl
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Replicas",type=string,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Image",type=string,priority=1,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workload is one deployable thing: an image, a port, and optionally a
// hostname. The platform renders it into the dozen Kubernetes objects a
// production workload needs, all of them hardened the same way, so that the
// safe configuration is the only configuration rather than the one a careful
// author remembers to write.
type Workload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadSpec   `json:"spec,omitempty"`
	Status WorkloadStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadList contains a list of Workload.
type WorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workload `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Workload{}, &WorkloadList{})
		return nil
	})
}
