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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
)

// The value every case here sends through, and the reason it looks like this:
// a space, a quote, an equals sign and a dollar. Each one is a character that a
// shell splits, strips or expands if the value is ever concatenated into a
// command line instead of passed as one argument.
const (
	awkwardValue = `a b "c" =d $HOME`

	// The two variables every case sends, and the list the script loops over.
	// Named because three files have to agree on them and a typo in one would
	// read as "the variable did not arrive".
	nodeEnv   = "NODE_ENV"
	nodeValue = "production"
	twoNames  = nodeEnv + " TRICKY"

	// The keys of the environment the script harness builds. Shared with
	// build_cache_test.go, which lays out the same tree.
	envPATH     = "PATH"
	envMeta     = "META"
	envBuildErr = "BUILD_ERR"
	envArgv     = "ARGV"

	// The builder this path is about, as the prepare step writes it down.
	methodDockerfile = "dockerfile"
)

func withBuildEnv(env ...platformv1alpha1.EnvVar) *platformv1alpha1.Build {
	return &platformv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "damga-build"},
		Spec: platformv1alpha1.BuildSpec{
			Repo: "https://example.test/acme/app.git", Revision: strings.Repeat("a", 40),
			Image: "registry.damga-registry.svc:5000/t/app", Env: env,
		},
	}
}

func envValue(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// Both containers carry them, because which one uses them is decided inside the
// pod: the prepare step reads the repository and writes down whether there is a
// Dockerfile. A container that turns out to need the variables and does not
// have them costs a build; one that carries variables it never reads costs
// nothing.
func TestBothContainersCarryTheBuildTimeVariables(t *testing.T) {
	job := desiredBuildJob(withBuildEnv(
		platformv1alpha1.EnvVar{Name: nodeEnv, Value: nodeValue},
		platformv1alpha1.EnvVar{Name: "TRICKY", Value: awkwardValue},
	))
	containers := append(
		append([]corev1.Container{}, job.Spec.Template.Spec.InitContainers...),
		job.Spec.Template.Spec.Containers...)
	if len(containers) != 2 {
		t.Fatalf("a detect build renders %d containers", len(containers))
	}
	for _, c := range containers {
		names, ok := envValue(c, envBuildArgs)
		if !ok {
			t.Fatalf("%s carries no %s, so the script has nothing to loop over", c.Name, envBuildArgs)
		}
		if names != twoNames {
			t.Errorf("%s lists %q", c.Name, names)
		}
		if got, _ := envValue(c, buildArgValuePrefix+"TRICKY"); got != awkwardValue {
			t.Errorf("%s carries %q, not the value as it was typed", c.Name, got)
		}
	}
}

// A variable called NAMES must not become the list of names.
//
// The first spelling of this was BUILD_ARG_NAMES for the list and
// BUILD_ARG_<name> for each value, and a variable called NAMES collides with it
// exactly. The build would then loop over that user's value as though it were a
// list of variable names — silently, producing an image built with neither the
// variables asked for nor an error.
func TestAVariableCalledNamesDoesNotBecomeTheList(t *testing.T) {
	job := desiredBuildJob(withBuildEnv(
		platformv1alpha1.EnvVar{Name: "NAMES", Value: "not a list"},
	))
	c := job.Spec.Template.Spec.Containers[0]
	if got, _ := envValue(c, envBuildArgs); got != "NAMES" {
		t.Fatalf("the list of names is %q", got)
	}
	if got, _ := envValue(c, buildArgValuePrefix+"NAMES"); got != "not a list" {
		t.Errorf("the value of NAMES is %q", got)
	}
}

func TestNoBuildTimeVariablesMeansNoneOfThoseVariables(t *testing.T) {
	job := desiredBuildJob(withBuildEnv())
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == envBuildArgs || strings.HasPrefix(e.Name, buildArgValuePrefix) {
				t.Errorf("%s carries %s for a build that asked for no variables", c.Name, e.Name)
			}
		}
	}
}

// The Dockerfile path, run for real against a recording builder.
//
// What is asserted is the argv buildctl was actually invoked with, because that
// is where the mistake would be: a value composed into one string rather than
// passed as one argument is split by the shell on the space in it, and the
// build then receives half a value and no error.
func TestABuildTimeVariableReachesTheDockerfileBuild(t *testing.T) {
	origin, head := dockerfileRepo(t)
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	recordingBuildctl(t, bin)

	env := map[string]string{
		envPATH:       bin + ":" + os.Getenv("PATH"),
		envRepo:       origin,
		envRevision:   head,
		envMethod:     methodDockerfile,
		envPathInRepo: "",
		envImage:      "registry.damga-registry.svc:5000/tenant-a/app:" + head,
		envCacheImage: testCacheRef,
		envWorkspace:  filepath.Join(root, "workspace"),
		envMeta:       filepath.Join(root, "meta.json"),
		envBuildErr:   filepath.Join(root, "build.err"),
		envResult:     filepath.Join(root, "result"),
		envArgv:       filepath.Join(root, "argv"),
		envHome:       root,

		envBuildArgs:                   twoNames,
		buildArgValuePrefix + nodeEnv:  nodeValue,
		buildArgValuePrefix + "TRICKY": awkwardValue,
	}
	run := runScript(t, buildScript, env)
	if run.code != 0 {
		t.Fatalf("the Dockerfile path failed with %d: %s\n%s", run.code, run.result, run.stderr)
	}

	argv, err := os.ReadFile(env[envArgv])
	if err != nil {
		t.Fatalf("the builder was never invoked: %v", err)
	}
	args := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
	for _, want := range []string{
		"build-arg:" + nodeEnv + "=" + nodeValue,
		"build-arg:TRICKY=" + awkwardValue,
	} {
		if !slices.Contains(args, want) {
			t.Errorf("buildctl was not asked for %q.\nIt was asked for:\n%s", want, argv)
		}
	}
	// One --opt per variable, and each one its own argument. A value that
	// arrived split across two arguments would still contain the text above in
	// the first of them, which is why this counts as well as matching.
	if got := slices.Index(args, "build-arg:TRICKY="+awkwardValue); got < 1 || args[got-1] != "--opt" {
		t.Errorf("the value is not preceded by its own --opt: %s", argv)
	}
}

func TestNoVariablesAddsNoFlagsToTheDockerfileBuild(t *testing.T) {
	origin, head := dockerfileRepo(t)
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	recordingBuildctl(t, bin)

	env := map[string]string{
		envPATH: bin + ":" + os.Getenv("PATH"), envRepo: origin, envRevision: head,
		envMethod: methodDockerfile, envPathInRepo: "",
		envImage:      "registry.damga-registry.svc:5000/tenant-a/app:" + head,
		envCacheImage: testCacheRef, envWorkspace: filepath.Join(root, "workspace"),
		envMeta: filepath.Join(root, "meta.json"), envBuildErr: filepath.Join(root, "build.err"),
		envResult: filepath.Join(root, "result"), envArgv: filepath.Join(root, "argv"),
		envHome: root,
	}
	if run := runScript(t, buildScript, env); run.code != 0 {
		t.Fatalf("the Dockerfile path failed with %d: %s\n%s", run.code, run.result, run.stderr)
	}
	argv, err := os.ReadFile(env[envArgv])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "build-arg:") {
		t.Errorf("a build with no variables still asked for one:\n%s", argv)
	}
}

// The buildpack path, and it is a different mechanism rather than the same one
// spelled differently.
//
// The CNB platform specification passes build-time variables as files under
// <platform>/env — one file per variable, the file's contents being the value —
// and the lifecycle exports them for the detector and the builder. A variable
// left in the container's own environment reaches neither, which is exactly the
// mistake this test exists to catch.
func TestABuildTimeVariableReachesTheBuildpackBuild(t *testing.T) {
	env := buildpackEnv(t, "buildpack")
	recordingLifecycle(t, env["LIFECYCLE"])
	platform := filepath.Join(filepath.Dir(env["LAYERS"]), "platform")
	env["PLATFORM"] = platform
	env[envBuildArgs] = twoNames
	env[buildArgValuePrefix+nodeEnv] = nodeValue
	env[buildArgValuePrefix+"TRICKY"] = awkwardValue

	run := runScript(t, buildpackScript, env)
	if run.code != 0 {
		t.Fatalf("the buildpack path failed with %d: %s\n%s", run.code, run.result, run.stderr)
	}

	for name, want := range map[string]string{nodeEnv: nodeValue, "TRICKY": awkwardValue} {
		body, err := os.ReadFile(filepath.Join(platform, "env", name))
		if err != nil {
			t.Fatalf("the lifecycle was given no %s: %v", name, err)
		}
		// Byte for byte, and the trailing newline is the point: the file's
		// contents are the value, so a redirect of printenv would hand the
		// buildpack a value ending in a line break.
		if string(body) != want {
			t.Errorf("%s reaches the build as %q, not %q", name, body, want)
		}
	}

	// And the phases were told where to look. The files are useless if the
	// lifecycle is pointed at its default directory instead.
	for _, phase := range []string{"detector", "builder"} {
		argv, err := os.ReadFile(filepath.Join(env["LIFECYCLE"], phase+".argv"))
		if err != nil {
			t.Fatalf("%s recorded nothing: %v", phase, err)
		}
		if !strings.Contains(string(argv), "-platform="+platform) {
			t.Errorf("the %s phase was not given the platform directory: %s", phase, argv)
		}
	}
}

// recordingLifecycle is fakeLifecycle plus the arguments each phase was given.
func recordingLifecycle(t *testing.T, dir string) {
	t.Helper()
	fakeLifecycle(t, dir, "")
	for _, phase := range lifecyclePhases {
		body, err := os.ReadFile(filepath.Join(dir, phase))
		if err != nil {
			t.Fatal(err)
		}
		record := "for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"" +
			filepath.Join(dir, phase+".argv") + "\"; done\n"
		if err := os.WriteFile(filepath.Join(dir, phase), append(body, []byte(record)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}
