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
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/placement"
)

// recordingDeliverer stands in for the cluster.
type recordingDeliverer struct {
	got  []placement.Placement
	note string
	err  error
}

func (d *recordingDeliverer) Deliver(_ context.Context, p placement.Placement) (string, error) {
	d.got = append(d.got, p)
	return d.note, d.err
}

// The link the product was missing, asserted at the point it is made: an
// install has to leave behind something that applies what it committed.
//
// Before this, the endpoint answered 201 for an install that would never run —
// correct manifests, in the right repository, watched by nothing. The
// installer's own list said so: "Argo CD. Nothing applies what a deploy
// commits."
func TestInstallingLeavesSomethingThatAppliesTheCommit(t *testing.T) {
	h := newHarness(t)
	h.stores.catalog = testCatalog(t)
	h.stores.writer = &gitwrite.Writer{Evidence: h.records}
	h.stores.gitAuth = pathAuth{}
	delivered := &recordingDeliverer{}
	h.stores.delivery = delivered
	repo := testBareRepo(t)

	code, body := h.install("shop", accMember, `{
		"template": "tidy",
		"repoUrl": "`+repo+`", "branch": "main", "path": "`+testInstallDir+`",
		"namespace": "`+nsHomeProd+`"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("installing = %d: %s", code, body)
	}

	if len(delivered.got) != 1 {
		t.Fatalf("the install delivered %d placements; without exactly one, the manifests it "+
			"committed are watched by nothing", len(delivered.got))
	}
	got := delivered.got[0]
	switch {
	case got.RepoURL != repo:
		t.Errorf("delivered repository %q, want %q", got.RepoURL, repo)
	case got.Path != testInstallDir:
		t.Errorf("delivered path %q; an Application pointed at the wrong directory applies "+
			"somebody else's manifests", got.Path)
	case got.Namespace != nsHomeProd:
		t.Errorf("delivered namespace %q", got.Namespace)
	case got.Branch != branchMain:
		t.Errorf("delivered revision %q, and the platform commits to %q", got.Branch, branchMain)
	}
}

// And the case that used to be the product's normal state, which now has to say
// so out loud rather than answering 201 and leaving it at that.
func TestAnInstallWithNothingToApplyItSaysSo(t *testing.T) {
	h := newHarness(t)
	h.stores.catalog = testCatalog(t)
	h.stores.writer = &gitwrite.Writer{Evidence: h.records}
	h.stores.gitAuth = pathAuth{}
	h.stores.delivery = nil // an install with no cluster to deliver into
	repo := testBareRepo(t)

	code, body := h.install("shop", accMember, `{
		"template": "tidy",
		"repoUrl": "`+repo+`", "branch": "main", "path": "`+testInstallDir+`",
		"namespace": "`+nsHomeProd+`"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("installing = %d: %s", code, body)
	}
	if !strings.Contains(body, "hiçbir şey onları uygulamıyor") {
		t.Fatalf("the response does not say that nothing will apply what was just committed. "+
			"That silence is the failure this whole change exists for: %s", body)
	}
}

// A delivery that fails must not undo an install that worked. The placement is
// written and the manifests are pushed; both survive, and the answer says which
// half is missing.
func TestADeliveryFailureDoesNotFailTheInstall(t *testing.T) {
	h := newHarness(t)
	h.stores.catalog = testCatalog(t)
	h.stores.writer = &gitwrite.Writer{Evidence: h.records}
	h.stores.gitAuth = pathAuth{}
	h.stores.delivery = &recordingDeliverer{err: errors.New("the API server refused it")}
	repo := testBareRepo(t)

	code, body := h.install("shop", accMember, `{
		"template": "tidy",
		"repoUrl": "`+repo+`", "branch": "main", "path": "`+testInstallDir+`",
		"namespace": "`+nsHomeProd+`"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("a failed delivery failed the whole install (%d); the manifests are committed "+
			"and telling the caller to retry would ask them to undo work that is fine: %s",
			code, body)
	}
	if !strings.Contains(body, "the API server refused it") {
		t.Errorf("the answer does not quote why delivery failed: %s", body)
	}
}

// The name has to be the same on the second call or delivery is not idempotent:
// a second install of the same app would leave two Applications watching one
// directory, and both would sync it.
func TestTheApplicationNameIsDerivedAndStable(t *testing.T) {
	place := placement.Placement{
		TenantID: "t_home", App: "shop", Env: envProd, Namespace: nsHomeProd,
	}
	first, second := applicationName(place), applicationName(place)
	if first != second {
		t.Fatalf("two calls named it %q and %q", first, second)
	}
	if !strings.Contains(first, place.Namespace) || !strings.Contains(first, place.App) {
		t.Errorf("the name %q is not derived from the pair the placement store keeps unique "+
			"(namespace, app), so two placements could produce one name", first)
	}

	// A different app in the same namespace is a different Application.
	other := place
	other.App = "worker"
	if applicationName(other) == first {
		t.Error("two apps in one namespace produced one Application name; one would overwrite " +
			"the other's source and apply the wrong directory")
	}
}
