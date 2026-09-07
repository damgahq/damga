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
	"net/http"
	"strings"
	"testing"

	"github.com/damgahq/damga/placement"
)

// The gap this closes, from the endpoint's side: a build-time variable was
// written into the manifest by the settings page and read by nothing. BuildSpec
// had no environment at all.

// buildable is a deployed app whose builds are recorded rather than run.
func buildable(t *testing.T) (*lifecycle, *recordingCreator) {
	t.Helper()
	l, _ := configured(t)
	created := &recordingCreator{}
	l.stores.builds = created
	return l, created
}

func setBuildEnv(t *testing.T, l *lifecycle, body string) {
	t.Helper()
	if code, out := l.settings(http.MethodPut, accMember, body); code != http.StatusAccepted {
		t.Fatalf("setting the build variables = %d: %s", code, out)
	}
}

func startBuild(t *testing.T, l *lifecycle, body string) (int, string) {
	t.Helper()
	return l.call(createBuild, http.MethodPost, "/apps/{app}/builds",
		tenantHome, appAPI, accMember, body)
}

// The whole chain, at the one place both halves meet: a variable typed into the
// settings page reaches the Build object that a job will read.
func TestABuildCarriesTheAppsBuildTimeVariables(t *testing.T) {
	l, created := buildable(t)
	setBuildEnv(t, l, `{"env":[
		{"key":"NODE_ENV","value":"production","build":true},
		{"key":"LOG_LEVEL","value":"debug","runtime":true}
	],"health":{},"resources":{}}`)

	code, body := startBuild(t, l, `{"repo":"https://example.test/acme/app.git",
		"revision":"`+strings.Repeat("b", 40)+`"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /builds = %d: %s", code, body)
	}
	if created.got == nil {
		t.Fatal("no build was created")
	}
	got := created.got.Spec.Env
	if len(got) != 1 || got[0].Name != "NODE_ENV" || got[0].Value != "production" {
		t.Fatalf("the build carries %v; it should carry the build-time half of the app's "+
			"settings and only that", got)
	}
}

// And the run-time half stays out of it. A variable that chooses what the
// process talks to has no business in an image layer.
func TestARuntimeVariableDoesNotReachTheBuild(t *testing.T) {
	l, created := buildable(t)
	setBuildEnv(t, l, `{"env":[{"key":"DATABASE_URL","value":"postgres://x","runtime":true}],
		"health":{},"resources":{}}`)

	if code, body := startBuild(t, l, `{"repo":"https://example.test/acme/app.git",
		"revision":"`+strings.Repeat("c", 40)+`"}`); code != http.StatusAccepted {
		t.Fatalf("POST /builds = %d: %s", code, body)
	}
	if got := created.got.Spec.Env; len(got) != 0 {
		t.Errorf("the build was given %v, which is what the running container reads and not "+
			"what the image is built with", got)
	}
}

// An app with one environment needs nobody to name it.
func TestOneEnvironmentNeedsNoChoice(t *testing.T) {
	l, created := buildable(t)
	setBuildEnv(t, l, `{"env":[{"key":"NODE_ENV","value":"production","build":true}],
		"health":{},"resources":{}}`)

	if code, body := startBuild(t, l, `{"repo":"https://example.test/acme/app.git",
		"revision":"`+strings.Repeat("d", 40)+`"}`); code != http.StatusAccepted {
		t.Fatalf("POST /builds = %d: %s", code, body)
	}
	if got := created.got.Spec.Env; len(got) != 1 {
		t.Errorf("the only environment's variables were not used: %v", got)
	}
}

// Two environments that disagree, and why this is a refusal rather than a
// choice made quietly.
//
// A build pushes to the repository plus the commit — the tag names the commit
// and nothing else — so two builds of one commit with different build-time
// variables push to the same tag and the second replaces the first. An image
// that depends on settings its own name does not carry is one nobody can reason
// about afterwards.
func TestTwoEnvironmentsThatDisagreeRefuseRatherThanPickOne(t *testing.T) {
	l, created := buildable(t)
	setBuildEnv(t, l, `{"env":[{"key":"NODE_ENV","value":"production","build":true}],
		"health":{},"resources":{}}`)
	secondEnvironment(t, l, "qa", `{"env":[{"key":"NODE_ENV","value":"test","build":true}],
		"health":{},"resources":{}}`)

	code, body := startBuild(t, l, `{"repo":"https://example.test/acme/app.git",
		"revision":"`+strings.Repeat("e", 40)+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("POST /builds = %d, want 409: %s", code, body)
	}
	for _, want := range []string{"same tag", envProd, "qa"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not mention %q: %s", want, body)
		}
	}
	if created.got != nil {
		t.Error("a build was started anyway, with one environment's settings")
	}

	// Naming one resolves it, and the answer is that environment's.
	code, body = startBuild(t, l, `{"env":"qa","repo":"https://example.test/acme/app.git",
		"revision":"`+strings.Repeat("e", 40)+`"}`)
	if code != http.StatusAccepted {
		t.Fatalf("naming the environment = %d: %s", code, body)
	}
	if got := created.got.Spec.Env; len(got) != 1 || got[0].Value != "test" {
		t.Errorf("the build used %v, not the environment that was named", got)
	}
}

// Two environments that agree need no choice either: there is nothing to
// choose between, and refusing would be a rule about the number of
// environments rather than about the ambiguity.
func TestTwoEnvironmentsThatAgreeAreNotAmbiguous(t *testing.T) {
	l, created := buildable(t)
	setBuildEnv(t, l, `{"env":[{"key":"NODE_ENV","value":"production","build":true}],
		"health":{},"resources":{}}`)
	secondEnvironment(t, l, "qa", `{"env":[{"key":"NODE_ENV","value":"production","build":true}],
		"health":{},"resources":{}}`)

	if code, body := startBuild(t, l, `{"repo":"https://example.test/acme/app.git",
		"revision":"`+strings.Repeat("f", 40)+`"}`); code != http.StatusAccepted {
		t.Fatalf("POST /builds = %d: %s", code, body)
	}
	if got := created.got.Spec.Env; len(got) != 1 || got[0].Value != "production" {
		t.Errorf("the build carries %v", got)
	}
}

func TestNamingAnEnvironmentThatDoesNotExistSaysSo(t *testing.T) {
	l, _ := buildable(t)
	code, body := startBuild(t, l, `{"env":"staging","repo":"https://example.test/acme/app.git",
		"revision":"`+strings.Repeat("a", 40)+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("POST /builds = %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "no environment called staging") {
		t.Errorf("the refusal is %s", body)
	}
}

// secondEnvironment places another environment of the same app, deploys it and
// gives it its own build-time settings.
func secondEnvironment(t *testing.T, l *lifecycle, env, settings string) {
	t.Helper()
	if _, err := l.places.Put(context.Background(), placement.Placement{
		TenantID: tenantHome, App: appAPI, Env: env,
		RepoURL: l.repo, Branch: branchMain, Path: lifeDir + "-" + env,
		Namespace: nsHomeProd + "-" + env,
	}); err != nil {
		t.Fatalf("placing %s: %v", env, err)
	}
	if code, body := l.callEnv(deployRoute, http.MethodPost, "/apps/{app}/envs/{env}/deploys",
		appAPI, env, accMember, `{"image":"`+imageOne+`"}`); code != http.StatusAccepted {
		t.Fatalf("deploying %s = %d: %s", env, code, body)
	}
	if code, body := l.callEnv(updateSettingsRoute, http.MethodPut,
		"/apps/{app}/envs/{env}/settings", appAPI, env, accMember, settings,
	); code != http.StatusAccepted {
		t.Fatalf("settings for %s = %d: %s", env, code, body)
	}
}
