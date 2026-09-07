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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// noBackups is the argument shape createDatabase passes when none were asked
// for.
func noBackups() (string, string, int32, bool) { return "", "", 0, false }

func someBackups(size string) func() (string, string, int32, bool) {
	return func() (string, string, int32, bool) { return "0 2 * * *", size, 7, true }
}

// Every refusal says which one it is.
//
// "that database is invalid" would be true of all of these at once, and each
// has a different next move: pick another name, pick an engine we run, pin the
// image, give a size, drop the field redis does not have. The CRD would refuse
// most of them too — but its refusal arrives at Argo CD minutes later, in a
// sync somebody has to go and read, which is the difference this list is for.
func TestADatabaseRequestIsRefusedByName(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func() (platformv1alpha1.Database, error)
		want []string
	}{
		{
			name: "no name",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("", "postgres", "postgres:17.2", "1Gi", "", "", false, noBackups)
			},
			want: []string{"ad gerekir"},
		},
		{
			name: "a name the API server would refuse",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("My_DB", "postgres", "postgres:17.2", "1Gi", "", "", false, noBackups)
			},
			want: []string{"My_DB", "küçük harfler"},
		},
		{
			name: "an engine this platform does not run",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("db", "mysql", "mysql:8", "1Gi", "", "", false, noBackups)
			},
			want: []string{"mysql", "postgres ve redis"},
		},
		{
			name: "an image that floats",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("db", "postgres", "postgres:latest", "1Gi", "", "", false, noBackups)
			},
			want: []string{"latest", "yeniden başlatmasını"},
		},
		{
			name: "an image with no tag at all",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("db", "postgres", "postgres", "1Gi", "", "", false, noBackups)
			},
			want: []string{"etiket ya da digest taşımıyor", "veri dizininin"},
		},
		{
			name: "storage that is not a size",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("db", "postgres", "postgres:17.2", "big", "", "", false, noBackups)
			},
			want: []string{"big", "1Gi", "küçültülemiyor"},
		},
		{
			name: "storage of zero",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("db", "postgres", "postgres:17.2", "0", "", "", false, noBackups)
			},
			want: []string{"sıfırdan büyük olmalı"},
		},
		{
			name: "a username redis has no use for",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("db", "redis", "redis:7.4.10", "1Gi", "", containerApp, false, noBackups)
			},
			want: []string{"redis", "numaralandırır"},
		},
		{
			name: "backups on redis, which would run pg_dump",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("db", "redis", "redis:7.4.10", "1Gi", "", "", true, someBackups("1Gi"))
			},
			want: []string{"redis", "pg_dump"},
		},
		{
			name: "a backup volume that is not a size",
			call: func() (platformv1alpha1.Database, error) {
				return databaseFrom("db", "postgres", "postgres:17.2", "1Gi", "", "", true, someBackups("plenty"))
			},
			want: []string{"plenty", "ayrıdır"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			if err == nil {
				t.Fatal("accepted, and the refusal would have arrived from admission " +
					"minutes later addressed to Argo CD")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q, so it does not name this "+
						"one: %v", want, err)
				}
			}
		})
	}
}

// And the ones that must be accepted, or the endpoint refuses its own product.
func TestADatabaseRequestIsAcceptedWhenItIsUsable(t *testing.T) {
	pg, err := databaseFrom("main", "postgres", "postgres:17.2", "5Gi", containerApp, containerApp, true, someBackups("2Gi"))
	if err != nil {
		t.Fatalf("a well formed postgres was refused: %v", err)
	}
	if pg.Spec.Engine != platformv1alpha1.EnginePostgres || pg.Spec.Database != containerApp {
		t.Errorf("the postgres fields did not survive: %+v", pg.Spec)
	}
	if pg.Spec.Backup == nil || pg.Spec.Backup.RetainDays != 7 {
		t.Errorf("the backup block did not survive: %+v", pg.Spec.Backup)
	}

	// An empty engine is postgres, which is the CRD's default and the thing
	// every Database was before there was a choice.
	implied, err := databaseFrom("main", "", "postgres:17.2", "1Gi", "", "", false, noBackups)
	if err != nil || implied.Spec.Engine != platformv1alpha1.EnginePostgres {
		t.Errorf("an unstated engine is not postgres: %v %v", implied.Spec.Engine, err)
	}

	if _, err := databaseFrom("cache", "redis", "redis:7.4.10", "1Gi", "", "", false, noBackups); err != nil {
		t.Errorf("a well formed redis was refused: %v", err)
	}
	// A digest instead of a tag is a pin too.
	if _, err := databaseFrom("main", "postgres",
		"postgres@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"1Gi", "", "", false, noBackups); err != nil {
		t.Errorf("a digest-pinned image was refused: %v", err)
	}
}

// What one database costs, which is not always one claim.
//
// Measured in the operator: databaseClaim is always rendered into the
// StatefulSet's volumeClaimTemplates, desiredBackupClaim is created only when
// Backup is set, and the API refuses backups on redis. The fence allows four.
func TestABackedUpDatabaseCostsTwoOfTheFourClaims(t *testing.T) {
	build := func(engine platformv1alpha1.DatabaseEngine, backup bool) platformv1alpha1.Database {
		db := platformv1alpha1.Database{Spec: platformv1alpha1.DatabaseSpec{Engine: engine}}
		if backup {
			db.Spec.Backup = &platformv1alpha1.DatabaseBackup{Storage: resource.MustParse("1Gi")}
		}
		return db
	}
	if got := claimsFor(build(platformv1alpha1.EnginePostgres, false)); got != 1 {
		t.Errorf("a postgres with no backups takes %d claims, want 1", got)
	}
	if got := claimsFor(build(platformv1alpha1.EnginePostgres, true)); got != 2 {
		t.Errorf("a backed-up postgres takes %d claims, want 2: the archives get a volume "+
			"of their own so that filling it cannot stop the database accepting writes", got)
	}
	if got := claimsFor(build(platformv1alpha1.EngineRedis, false)); got != 1 {
		t.Errorf("a redis takes %d claims, want 1", got)
	}
	// The number that matters: two backed-up databases are the whole budget,
	// and they share it with every application volume in the namespace.
	if claimsFor(build(platformv1alpha1.EnginePostgres, true))*2 != claimBudget {
		t.Errorf("two backed-up databases no longer exhaust the %d-claim fence; the "+
			"endpoint's advice about what this costs is now wrong", claimBudget)
	}
}

// call drives a database route through a mux, so the path values are set the
// way the router sets them. The lifecycle harness's own helper only does POST.
func (l *lifecycle) db(
	handler func(guard, stores) http.Handler, method, suffix, name, account, body string,
) (int, string) {
	l.t.Helper()
	mux := http.NewServeMux()
	mux.Handle(method+" "+tenantScope+suffix, handler(l.guard, l.stores))
	target := strings.NewReplacer(
		"{tenant}", tenantHome, "{app}", appAPI, "{env}", envProd, "{database}", name,
	).Replace(tenantScope + suffix)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = testHost
	if account != "" {
		req.AddCookie(l.cookies[account])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

const dbSuffix = "/apps/{app}/envs/{env}/databases"
const oneDatabase = `{"name":"main","engine":"postgres","image":"postgres:17.2","storage":"5Gi"}`

// The whole point of the item: one request, and the thing exists in git.
func TestADatabaseIsCommittedAndThenListed(t *testing.T) {
	l := newLifecycle(t)

	code, body := l.db(createDatabase, http.MethodPost, dbSuffix, "", accOwner, oneDatabase)
	if code != http.StatusAccepted {
		t.Fatalf("creating a database answered %d: %s", code, body)
	}

	code, body = l.db(listDatabases, http.MethodGet, dbSuffix, "", accOwner, "")
	if code != http.StatusOK {
		t.Fatalf("listing answered %d: %s", code, body)
	}
	if !strings.Contains(body, `"name":"main"`) {
		t.Errorf("the database that was just created is not in the list. A create "+
			"followed by a list that shows nothing reads as a create that failed: %s", body)
	}
	if !strings.Contains(body, `"claims":1`) {
		t.Errorf("the list does not say what the database costs: %s", body)
	}
	if !strings.Contains(body, `"budget":4`) {
		t.Errorf("the list does not say what the ceiling is: %s", body)
	}
	// The one field that must never appear.
	// Named once: three spellings of the same literal in one file is what
	// goconst is for, and this list is the assertion rather than the words in
	// it.
	const secretish = "password"
	for _, forbidden := range []string{secretish, strings.ToUpper(secretish[:1]) + secretish[1:],
		"connectionString", "dsn"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the list carries %q. Credentials are minted into a Secret the "+
				"tenant's pods read; this endpoint reads committed manifests, which "+
				"never held them: %s", forbidden, body)
		}
	}
}

func TestASecondDatabaseWithTheSameNameIsRefused(t *testing.T) {
	l := newLifecycle(t)
	if code, body := l.db(createDatabase, http.MethodPost, dbSuffix, "", accOwner, oneDatabase); code != http.StatusAccepted {
		t.Fatalf("the first create answered %d: %s", code, body)
	}
	code, body := l.db(createDatabase, http.MethodPost, dbSuffix, "", accOwner, oneDatabase)
	if code != http.StatusConflict {
		t.Fatalf("a duplicate name answered %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "adlı bir veritabanı var") {
		t.Errorf("the refusal does not say what the conflict is: %s", body)
	}
}

// Removing one, and saying what that does to the data.
//
// "Removed" and "the data is gone" are different events, and the response has
// to carry the difference: the volumes stay, they stay spent against the
// namespace's four claims, and the same name later attaches to them.
func TestRemovingADatabaseSaysWhatBecomesOfTheData(t *testing.T) {
	l := newLifecycle(t)
	// Deployed first, which is the shape this happens in: manifest.Fence puts
	// the namespace and the quota in the same directory as the workload, so a
	// deployed app's directory is never left empty by removing one database.
	// An app that was never deployed is the other case and has its own
	// refusal — see TestRemovingTheOnlyManifestSaysSoRatherThanFailing.
	if code, body := l.deploy(`{"image":"` + imageOne + `"}`); code != http.StatusAccepted {
		t.Fatalf("the setup deploy answered %d: %s", code, body)
	}
	if code, body := l.db(createDatabase, http.MethodPost, dbSuffix, "", accOwner, oneDatabase); code != http.StatusAccepted {
		t.Fatalf("create answered %d: %s", code, body)
	}

	code, body := l.db(deleteDatabase, http.MethodDelete, dbSuffix+"/{database}", "main", accOwner, "")
	if code != http.StatusAccepted {
		t.Fatalf("removing answered %d: %s", code, body)
	}
	for _, want := range []string{"Veri volume'ü", "yedek volume'ü", "dört claim", "bağlanacak"} {
		if !strings.Contains(body, want) {
			t.Errorf("the response does not say %q. Somebody removing a database to free "+
				"space would learn none of it from this: %s", want, body)
		}
	}

	code, body = l.db(listDatabases, http.MethodGet, dbSuffix, "", accOwner, "")
	if code != http.StatusOK || strings.Contains(body, `"name":"main"`) {
		t.Errorf("the database is still listed after being removed: %d %s", code, body)
	}
	// And the workload committed beside it is untouched. gitwrite deletes what
	// the render omitted and Owns says these files are ours, so a render that
	// returned only the remaining databases would take the application with it.
	if !strings.Contains(body, `"namespace"`) {
		t.Errorf("the list stopped answering at all: %s", body)
	}
}

func TestRemovingADatabaseThatIsNotThereIsNotAServerError(t *testing.T) {
	l := newLifecycle(t)
	if code, _ := l.db(createDatabase, http.MethodPost, dbSuffix, "", accOwner, oneDatabase); code != http.StatusAccepted {
		t.Fatal("setup failed")
	}
	code, body := l.db(deleteDatabase, http.MethodDelete, dbSuffix+"/{database}", "ghost", accOwner, "")
	if code != http.StatusNotFound {
		t.Fatalf("removing a database that is not committed answered %d, want 404: %s", code, body)
	}
	if !strings.Contains(body, "ghost") {
		t.Errorf("the refusal does not name what was not found: %s", body)
	}
}

// Creating is the deploy right; removing is not.
func TestRemovingADatabaseIsOwnerOnly(t *testing.T) {
	l := newLifecycle(t)

	// A member may create one: they can already deploy a Workload with a
	// volume, which takes a claim the same way.
	if code, body := l.db(createDatabase, http.MethodPost, dbSuffix, "", accMember, oneDatabase); code != http.StatusAccepted {
		t.Errorf("a member could not create a database (%d), though a member may deploy "+
			"an application that writes to disk: %s", code, body)
	}
	if code, _ := l.db(createDatabase, http.MethodPost, dbSuffix, "", accViewer, oneDatabase); code != http.StatusForbidden {
		t.Errorf("a viewer created a database: %d", code)
	}
	for _, who := range []string{accMember, accViewer} {
		code, body := l.db(deleteDatabase, http.MethodDelete, dbSuffix+"/{database}", "main", who, "")
		if code != http.StatusForbidden {
			t.Errorf("%s removed a database and got %d. Unregistering an app removes a "+
				"row; this withdraws the manifest holding a StatefulSet up: %s",
				who, code, body)
		}
	}
}

// An app with a database and nothing else cannot have the database removed,
// and the refusal says what to do instead.
//
// gitwrite refuses a render that produces no files, and it is right to: an
// empty directory is indistinguishable from a render that went wrong, and this
// package deletes what a render omits. So the last manifest cannot be
// withdrawn through here. The way out already exists and is what the message
// names — unregistering the app removes the row and leaves the manifests
// committed and readable.
//
// It is a corner rather than the common case: a deployed app's directory also
// holds the namespace and the quota that manifest.Fence writes beside the
// workload, so removing a database from one is never the last file.
func TestRemovingTheOnlyManifestSaysSoRatherThanFailing(t *testing.T) {
	l := newLifecycle(t)
	if code, body := l.db(createDatabase, http.MethodPost, dbSuffix, "", accOwner, oneDatabase); code != http.StatusAccepted {
		t.Fatalf("create answered %d: %s", code, body)
	}
	code, body := l.db(deleteDatabase, http.MethodDelete, dbSuffix+"/{database}", "main", accOwner, "")
	if code != http.StatusConflict {
		t.Fatalf("removing the only manifest answered %d, want 409: %s", code, body)
	}
	for _, want := range []string{"commit edilmiş tek manifest", "uygulamanın kaydını silin"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not say %q, so it names the problem and not the "+
				"way out: %s", want, body)
		}
	}
}
