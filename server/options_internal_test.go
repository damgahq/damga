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

// In package server because withDefaults is unexported, and withDefaults is
// half of what these cases are about: a value with two default sites is a value
// that disagrees with itself later.
package server

import (
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// bind returns a Config with the flags parsed, and a FlagSet that does not
// write to stderr or exit the test binary on a bad flag.
func bind(t *testing.T, args ...string) Config {
	t.Helper()
	var c Config
	fs := flag.NewFlagSet("damga", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return c
}

// The registry is configuration, and an install that sets it gets what it set.
//
// It was a constant in server/builds.go until this case existed, which made an
// install with a registry of its own name an image on every single request —
// there was no way to change the default, only a way to avoid it.
func TestRegistryComesFromTheFlag(t *testing.T) {
	got := bind(t, "-registry", "registry.example.test:5000").Registry
	if got != "registry.example.test:5000" {
		t.Errorf("Registry = %q, want what -registry said", got)
	}
}

// The default has two sites — the flag, for anything with a command line, and
// withDefaults, for an Options built in Go — and they have to be one value.
//
// This is the case worth having, and it is written because the shape has cost
// this repository twice: buildHome was spelled in three places and two CI
// rounds went to them disagreeing, and config/crd/kustomization.yaml was a
// hand-maintained list that named two of three CRDs, so the operator
// reconciled a type the cluster had never been told about. Neither was caught
// by anything reading the code.
func TestTheRegistryDefaultIsOneValue(t *testing.T) {
	fromFlag := bind(t).Registry
	fromOptions := Options{}.withDefaults().Config.Registry

	if fromFlag != fromOptions {
		t.Errorf("the two defaults disagree: the flag says %q, withDefaults says %q",
			fromFlag, fromOptions)
	}
	// And neither is empty, which is the way they could agree and both be
	// wrong: a build with no registry composes an image reference that begins
	// with a slash, which nothing rejects until the push fails.
	if fromFlag == "" {
		t.Error("the registry defaults to nothing, so a build would push to /tenant/app")
	}
}

// An install that passes an empty registry gets the default rather than a
// broken image reference. The flag can be set to "" explicitly, and Options
// literals leave it zero far more often than they set it.
func TestAnEmptyRegistryFallsBackRatherThanComposingASlash(t *testing.T) {
	o := Options{Config: Config{Registry: ""}}.withDefaults()
	if o.Config.Registry != defaultRegistry {
		t.Errorf("Registry = %q, want the default", o.Config.Registry)
	}

	build, err := buildFor(o.Config.Registry, tenantHome, appAPI, nil, createBuildRequest{
		Repo: sourceRepo, Revision: testRevision,
	})
	if err != nil {
		t.Fatalf("buildFor: %v", err)
	}
	if want := defaultRegistry + "/" + tenantHome + "/" + appAPI; build.Spec.Image != want {
		t.Errorf("Image = %q, want %q", build.Spec.Image, want)
	}
}

// The registry reaches a build through the handler Run builds, and not only
// through a stores a test assembled.
//
// Everything above this drives handlers with a stores written out by hand,
// which is what makes them readable and is also what they cannot check: that
// Options.handler copies Config.Registry into it at all. Deleting that one
// assignment leaves every other case in this package passing. The same is true
// of Options.Builds, so this goes through both.
func TestTheHandlerCarriesTheConfiguredRegistryAndTheBuildSeam(t *testing.T) {
	h := newHarness(t)
	creator := &recordingCreator{}

	o := Options{
		Config:    Config{Registry: testRegistry, SessionTTL: time.Hour},
		Evidence:  h.records,
		Identity:  h.idStore,
		Placement: h.places,
		Builds:    creator,
		GitAuth:   noAuth{},
	}.withDefaults()
	handler, err := o.handler(h.records, h.idStore)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/tenants/"+tenantHome+"/apps/"+appAPI+"/builds",
		strings.NewReader(`{"repo":"`+sourceRepo+`","revision":"`+testRevision+`"}`))
	req.Host = testHost
	req.AddCookie(h.cookies[accMember])
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /builds through the real handler = %d: %s", rec.Code, rec.Body.String())
	}
	if creator.got == nil {
		t.Fatal("Options.Builds was never reached: the seam is not wired into stores")
	}
	if want := testRegistry + "/" + tenantHome + "/" + appAPI; creator.got.Spec.Image != want {
		t.Errorf("Image = %q, want %q — Config.Registry did not reach the handler",
			creator.got.Spec.Image, want)
	}
}
