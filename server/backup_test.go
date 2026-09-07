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

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/damgahq/damga/identity"
	"github.com/damgahq/damga/placement"
	placementmem "github.com/damgahq/damga/placement/memory"
	"github.com/damgahq/damga/server"
)

type fakeBackups struct {
	status server.BackupStatus
	err    error
	seen   []string
}

func (f *fakeBackups) ForApp(_ context.Context, namespace, app string) (server.BackupStatus, error) {
	f.seen = append(f.seen, namespace+"/"+app)
	return f.status, f.err
}

// The database every case here names.
const testDatabase = "shop-db"

func backupURL(base string) string {
	return fmt.Sprintf("%s/api/v1/tenants/%s/apps/%s/envs/%s/backup",
		base, testTenant, testApp, testEnv)
}

func withPlacement(t *testing.T, backups server.BackupReader) (string, *http.Cookie) {
	t.Helper()
	places := placementmem.New()
	if _, err := places.Put(t.Context(), placement.Placement{
		TenantID: testTenant, App: testApp, Env: testEnv,
		RepoURL: "https://example.test/state.git", Branch: testBranch,
		Path: testPath, Namespace: testNamespace,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	base := start(t, server.Options{
		Placement: places, Backups: backups,
		Identity: identityWith(t, identity.RoleOwner),
	})
	return base, login(t, base)
}

// The line the whole restore rehearsal exists to support, end to end.
func TestTheBackupLineSaysWhenAndHowMany(t *testing.T) {
	f := &fakeBackups{status: server.BackupStatus{
		Database: testDatabase, Rehearsed: true,
		FinishedAt: time.Date(2026, 8, 29, 2, 4, 11, 0, time.UTC),
		Rows:       1284, SourceRows: 1284, Tables: 7,
	}}
	base, cookie := withPlacement(t, f)

	code, body := get(t, backupURL(base), cookieHeader(cookie))
	if code != http.StatusOK {
		t.Fatalf("GET backup = %d %q", code, body)
	}
	for _, want := range []string{
		`"state":"restored"`, `"rows":1284`, `"sourceRows":1284`,
		`"database":"` + testDatabase + `"`, "2026-08-29T02:04:11Z",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer has no %s: %s", want, body)
		}
	}

	// The namespace came from the placement and not from a convention. A
	// namespace derived from the tenant and the environment is a name parsed
	// out of an identity, which placement carries a field to avoid.
	if len(f.seen) != 1 || f.seen[0] != testNamespace+"/"+testApp {
		t.Errorf("the reader was asked about %v", f.seen)
	}
}

// Three answers, not two. A database whose first backup has not run yet is
// neither a success nor a failure, and a page with only two states has to call
// it one of them.
func TestABackupThatHasNotRunYetIsItsOwnAnswer(t *testing.T) {
	base, cookie := withPlacement(t, &fakeBackups{
		status: server.BackupStatus{Database: testDatabase},
	})

	code, body := get(t, backupURL(base), cookieHeader(cookie))
	if code != http.StatusOK {
		t.Fatalf("GET backup = %d %q", code, body)
	}
	if !strings.Contains(body, `"state":"none yet"`) {
		t.Errorf("a database with no run yet was reported as something else: %s", body)
	}
	if strings.Contains(body, `"finishedAt":"`) {
		t.Errorf("a time was reported for a run that has not happened: %s", body)
	}
}

// An install that turned the rehearsal off still has backups, and saying so is
// more honest than a blank line — but it must not say "restored".
func TestABackupWithoutARehearsalSaysWhichItIs(t *testing.T) {
	base, cookie := withPlacement(t, &fakeBackups{status: server.BackupStatus{
		Database: testDatabase, Rehearsed: false,
		FinishedAt: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC),
	}})

	_, body := get(t, backupURL(base), cookieHeader(cookie))
	if !strings.Contains(body, `"state":"backed up"`) {
		t.Errorf("state = something other than \"backed up\": %s", body)
	}
	if strings.Contains(body, "restored") {
		t.Errorf("an archive nothing restored was reported as restored: %s", body)
	}
}

func TestAnAppWithNoDatabaseIsNotAnError(t *testing.T) {
	base, cookie := withPlacement(t, &fakeBackups{err: server.ErrNoDatabase})

	code, body := get(t, backupURL(base), cookieHeader(cookie))
	if code != http.StatusNotFound {
		t.Errorf("an app with no database = %d, want 404", code)
	}
	if !strings.Contains(body, "bir veritabanına bağlı değil") {
		t.Errorf("the answer does not say which of the two it is: %s", body)
	}
}

// An install with no cluster to read says so. Answering with an empty status
// would read as "no backups", which is the one thing it must not be mistaken
// for.
func TestAnInstallThatCannotReadTheClusterSaysSo(t *testing.T) {
	base, cookie := withPlacement(t, nil)

	code, body := get(t, backupURL(base), cookieHeader(cookie))
	if code != http.StatusNotImplemented {
		t.Errorf("= %d %q, want 501", code, body)
	}
}
