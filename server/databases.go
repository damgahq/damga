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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// The claim budget, and why this endpoint states it rather than enforcing it.
//
// A tenant namespace is fenced at four persistent volume claims
// (internal/manifest/fence.go). A Database takes one for its data, and a second
// for its archives when backups are on — measured in the operator:
// databaseClaim is always rendered, desiredBackupClaim only when Backup is set,
// and the API refuses backups on redis outright. So two backed-up Postgres
// databases use the whole budget, and they share it with every application
// volume in the same namespace.
//
// It cannot predict the refusal, and that is three separate findings rather
// than one:
//
//   - The control plane may not look. Its ClusterRole grants pods, pods/log,
//     pods/exec, deployments, workloads and databases; it carries no rule for
//     persistentvolumeclaims and none for resourcequotas, so live usage is not
//     readable even in principle.
//   - Counting the committed manifests instead would undercount. placement.go
//     says a tenant "may put as many apps in one [namespace] as it likes", and
//     each app's manifests live in its own directory — so the one directory
//     this endpoint reads is not the namespace.
//   - The refusal does not happen during the request anyway. The write path is
//     git: this returns once a commit is pushed, and the quota is applied by
//     the API server when Argo CD syncs. gitwrite says it plainly where it
//     returns a pending record — "this package knows a commit was pushed and
//     nothing more".
//
// So the honest thing is a fact rather than a forecast: what this object will
// take, and what the ceiling is. The refusal, when it comes, arrives where
// every other rejected sync arrives.
const (
	claimBudget      = 4
	claimsForData    = 1
	claimsForBackups = 1
)

// The refusals this endpoint tells apart. One message for all of them would be
// true of each and useful for none: the next move is different every time.
var (
	errDatabaseExists = errors.New("bu veritabanı burada zaten var")
	errNoSuchDatabase = errors.New("bu adda bir veritabanı burada commit edilmemiş")
	// errLastManifest: removing this file would leave the directory empty, and
	// gitwrite refuses an empty render because it cannot tell one from a render
	// that failed. Reported as itself rather than as "nothing to write".
	errLastManifest = errors.New(
		"burada commit edilmiş tek manifest bu, ve onu kaldırmak dizini boş bırakırdı; " +
			"onun yerine uygulamanın kaydını silin")
)

// databaseFrom validates a request into an object the API server will accept.
//
// Checked here and not left to admission, because the two refusals do not
// arrive in the same place. A field this function rejects is answered in the
// response to the request that set it; a field admission rejects is answered
// to Argo CD, minutes later, in a sync somebody has to go and read. The CRD
// keeps its rules either way — this is not a second copy of them, it is the
// subset that can be checked before a commit exists.
func databaseFrom(
	name, engine, image, storage, database, username string,
	wantBackups bool, backups func() (string, string, int32, bool),
) (platformv1alpha1.Database, error) {
	var zero platformv1alpha1.Database
	switch {
	case name == "":
		return zero, errors.New("bir veritabanı için ad gerekir")
	case !databaseName.MatchString(name):
		return zero, fmt.Errorf(
			"%q kullanılabilir bir ad değil: küçük harfler, rakamlar ve tireler, "+
				"başı ve sonu harf ya da rakam", name)
	case len(name) > 63:
		return zero, fmt.Errorf("%q bir adın alabileceği 63 karakterden uzun", name)
	}

	kind := platformv1alpha1.DatabaseEngine(engine)
	if engine == "" {
		kind = platformv1alpha1.EnginePostgres
	}
	if kind != platformv1alpha1.EnginePostgres && kind != platformv1alpha1.EngineRedis {
		return zero, fmt.Errorf(
			"%q bu platformun çalıştırdığı bir motor değil: postgres ve redis çalıştırılıyor", engine)
	}

	if image == "" {
		return zero, errors.New("bir veritabanı için bir etikete ya da digest'e sabitlenmiş bir imaj gerekir")
	}
	// The CRD's own rule, said in the response instead of in a sync. A tag that
	// moves is the thing the Database type spends its longest comment on:
	// PostgreSQL will not start on a directory a newer major wrote, and Redis
	// destroys the way back from an upgrade rather than the upgrade.
	if strings.HasSuffix(image, ":latest") {
		return zero, errors.New(
			"imaj :latest kullanmamalı — kayan bir etiket, bir sonraki pod yeniden " +
				"başlatmasını, hiçbir şeyin buna sebep olan değişiklikle ilişkilendiremediği bir kesintiye çevirir")
	}
	if !strings.Contains(image, "@") {
		last := image[strings.LastIndex(image, "/")+1:]
		if !strings.Contains(last, ":") {
			return zero, fmt.Errorf(
				"%q imajı etiket ya da digest taşımıyor, yani çalıştırdığı şey veri "+
					"dizininin altında değişirdi", image)
		}
	}

	size, err := resource.ParseQuantity(storage)
	if err != nil {
		return zero, fmt.Errorf(
			"%q bir boyut değil: 1Gi ve 512Mi birer boyut. Depolamanın varsayılanı yok, "+
				"çünkü sonradan küçültülemiyor", storage)
	}
	if size.IsZero() || size.Sign() < 0 {
		return zero, errors.New("depolama sıfırdan büyük olmalı")
	}

	db := platformv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: platformv1alpha1.DatabaseSpec{
			Engine: kind, Image: image, Storage: size,
		},
	}
	if kind == platformv1alpha1.EnginePostgres {
		db.Spec.Database = database
		db.Spec.Username = username
	} else if database != "" || username != "" {
		// Refused rather than dropped. Redis numbers its databases and
		// authenticates without a user, so these two would be accepted,
		// committed, and mean nothing — a setting that is stored and does
		// nothing, which is the failure the settings page exists to prevent.
		return zero, errors.New(
			"redis ne veritabanı adı ne de kullanıcı adı alır: veritabanlarını " +
				"numaralandırır ve yalnızca parolayla kimlik doğrular")
	}

	if wantBackups {
		if kind == platformv1alpha1.EngineRedis {
			return zero, errors.New(
				"redis'in burada yedeği yok: zamanlama, pg_dump'ı onu hiç duymamış bir " +
					"sunucuya karşı çalıştırırdı")
		}
		schedule, backupSize, retain, rehearse := backups()
		archive, err := resource.ParseQuantity(backupSize)
		if err != nil || archive.IsZero() || archive.Sign() < 0 {
			return zero, fmt.Errorf(
				"yedek volume'ü %q sıfırdan büyük bir boyut değil; veri volume'ünden "+
					"ayrıdır, böylece dolması yazmaları durduramaz", backupSize)
		}
		db.Spec.Backup = &platformv1alpha1.DatabaseBackup{
			Schedule: schedule, RetainDays: retain, Storage: archive, Rehearse: &rehearse,
		}
	}
	return db, nil
}

// databaseName is the RFC 1123 label rule, which is what the API server will
// apply to the object this renders. Checked here so the refusal names the
// field rather than arriving from a sync three minutes later.
var databaseName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// wireDatabase is what this endpoint says about a database.
//
// No password, no connection string, and no field that could carry one. The
// operator mints the credentials into a Secret the tenant's pods read; this
// endpoint reads committed manifests, which never held them. Saying so here
// rather than only in the panel, because the shape is the guarantee: there is
// no field to fill in wrongly later.
type wireDatabase struct {
	Name     string `json:"name"`
	Engine   string `json:"engine"`
	Image    string `json:"image"`
	Storage  string `json:"storage"`
	Database string `json:"database,omitempty"`
	Username string `json:"username,omitempty"`

	// Backups is absent rather than false-y when there are none, so that "no
	// backups" and "backups whose schedule this build cannot read" are not the
	// same JSON.
	Backups *wireDatabaseBackup `json:"backups,omitempty"`

	// SecretName is where the credentials are, not what they are. It is the
	// same thing the settings page shows for a secret: git carries which
	// Secret to look in and never the value.
	SecretName string `json:"secretName"`

	// Claims is what this object takes out of the namespace's budget.
	Claims int `json:"claims"`
}

// Named for the database rather than for backups in general: server/backup.go
// already has a wireBackup, and that one is a Workload's backup status.
type wireDatabaseBackup struct {
	Schedule   string `json:"schedule"`
	RetainDays int32  `json:"retainDays"`
	Storage    string `json:"storage"`
	Rehearse   bool   `json:"rehearse"`
}

func toWireDatabase(db platformv1alpha1.Database) wireDatabase {
	out := wireDatabase{
		Name: db.Name, Engine: string(db.Spec.Engine), Image: db.Spec.Image,
		Storage: db.Spec.Storage.String(), Database: db.Spec.Database,
		Username: db.Spec.Username, SecretName: db.Name, Claims: claimsFor(db),
	}
	if db.Spec.Backup != nil {
		out.Backups = &wireDatabaseBackup{
			Schedule: db.Spec.Backup.Schedule, RetainDays: db.Spec.Backup.RetainDays,
			Storage: db.Spec.Backup.Storage.String(),
			// A nil Rehearse is the CRD's default, which is on. Reported as what
			// it will do rather than as the absence of an answer.
			Rehearse: db.Spec.Backup.Rehearse == nil || *db.Spec.Backup.Rehearse,
		}
	}
	return out
}

// claimsFor is the measured cost of one Database, and it is not always one.
func claimsFor(db platformv1alpha1.Database) int {
	if db.Spec.Backup != nil && db.Spec.Engine != platformv1alpha1.EngineRedis {
		return claimsForData + claimsForBackups
	}
	return claimsForData
}

// committedDatabases reads the databases already in the tenant's repository.
//
// From git and not from the cluster, and the reason is what a person does next.
// The cluster answers what Argo CD has applied so far; a database created a
// moment ago is not in it, so a create followed by a list would show nothing
// and read as a create that failed. Git answers what this app is configured to
// have, which is the question this endpoint's own writes change.
func committedDatabases(files map[string][]byte) []platformv1alpha1.Database {
	var out []platformv1alpha1.Database
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if db, ok := manifest.ParseDatabase(files[name]); ok {
			out = append(out, db)
		}
	}
	return out
}

// listDatabases answers what this app environment is configured to run.
func listDatabases(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionAppView)
		if !ok {
			return
		}
		place, files, done := readCommitted(w, r, st, ref)
		if !done {
			return
		}
		dbs := committedDatabases(files)
		wire := make([]wireDatabase, 0, len(dbs))
		used := 0
		for _, db := range dbs {
			wire = append(wire, toWireDatabase(db))
			used += claimsFor(db)
		}
		writeJSON(w, map[string]any{
			"databases":    wire,
			fieldNamespace: place.Namespace,
			// Stated, not enforced. See the comment on claimBudget for the
			// three reasons this is a fact about this directory rather than a
			// prediction about the namespace.
			"claims": map[string]any{
				"budget":            claimBudget,
				"usedByTheseHere":   used,
				"sharedWithTheApps": true,
			},
		})
	})
}

// readCommitted clones once and hands back the directory.
func readCommitted(
	w http.ResponseWriter, r *http.Request, st stores, ref evidence.Ref,
) (placement.Placement, map[string][]byte, bool) {
	place, err := st.placement.Get(r.Context(), ref.TenantID, ref.App, ref.Env)
	switch {
	case errors.Is(err, placement.ErrNotFound):
		problem(w, http.StatusNotFound, "bu uygulama ve ortam için henüz bir depo tanımlı değil")
		return placement.Placement{}, nil, false
	case err != nil:
		problem(w, http.StatusInternalServerError, "yerleşim kaydı okunamadı")
		return placement.Placement{}, nil, false
	}
	if st.writer == nil {
		problem(w, http.StatusNotImplemented, "bu kurulumda tanımlı bir git yazıcısı yok")
		return placement.Placement{}, nil, false
	}
	method, err := st.gitAuth.For(place.RepoURL)
	if err != nil {
		problem(w, http.StatusInternalServerError, err.Error())
		return placement.Placement{}, nil, false
	}
	files, err := st.writer.Read(r.Context(), gitwrite.Target{
		RepoURL: place.RepoURL, Branch: place.Branch, Dir: place.Path, Auth: method,
	})
	if err != nil {
		problem(w, http.StatusBadGateway, "depo okunamadı: "+err.Error())
		return placement.Placement{}, nil, false
	}
	return place, files, true
}

// createDatabase commits a Database into the tenant's repository.
func createDatabase(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ref, ok := g.admit(w, r, authz.ActionDatabaseCreate)
		if !ok {
			return
		}
		var req struct {
			Name     string `json:"name"`
			Engine   string `json:"engine"`
			Image    string `json:"image"`
			Storage  string `json:"storage"`
			Database string `json:"database"`
			Username string `json:"username"`
			Backups  *struct {
				Schedule   string `json:"schedule"`
				RetainDays int32  `json:"retainDays"`
				Storage    string `json:"storage"`
				Rehearse   bool   `json:"rehearse"`
			} `json:"backups"`
		}
		if err := json.NewDecoder(
			http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "istek okunamadı: "+err.Error())
			return
		}

		db, err := databaseFrom(req.Name, req.Engine, req.Image, req.Storage,
			req.Database, req.Username, req.Backups != nil, func() (string, string, int32, bool) {
				if req.Backups == nil {
					return "", "", 0, false
				}
				return req.Backups.Schedule, req.Backups.Storage, req.Backups.RetainDays, req.Backups.Rehearse
			})
		if err != nil {
			problem(w, http.StatusBadRequest, err.Error())
			return
		}

		commitChange(w, r, st, sub, ref, "add database "+db.Name,
			func(place placement.Placement) renderFunc {
				return func(_ string, current map[string][]byte) (map[string][]byte, error) {
					for _, existing := range committedDatabases(current) {
						if existing.Name == db.Name {
							return nil, fmt.Errorf(
								"%w: bu ortamda zaten %q adlı bir veritabanı var",
								errDatabaseExists, db.Name)
						}
					}
					db.Namespace = place.Namespace
					body, err := manifest.RenderDatabase(db)
					if err != nil {
						return nil, err
					}
					// Everything already committed, plus one file. A render
					// that returned only the new database would have gitwrite
					// remove the workload beside it — Owns says these files are
					// ours, and ours is exactly what it is willing to delete.
					out := make(map[string][]byte, len(current)+1)
					maps.Copy(out, current)
					out[manifest.FileFor("Database", db.Name)] = body
					return out, nil
				}
			},
			// No removal: this render returns everything already committed and
			// one file more, so there is nothing for gitwrite to withdraw, and
			// a caller that does not need the power should not hold it.
			commitOptions{})
	})
}

// deleteDatabase withdraws the manifest, and says what that does to the data.
//
// "Removed" and "the data is gone" are deliberately not the same sentence,
// because they are not the same event and the difference is measurable:
//
//   - The Database object goes, and with it the StatefulSet, the Service and
//     the Secret holding the credentials. Those are owned, so the garbage
//     collector takes them.
//   - Both volumes stay. Kubernetes leaves a StatefulSet's volumeClaimTemplate
//     claims behind on purpose, and the operator creates the archive claim
//     unowned for exactly this reason — its comment says an owner reference
//     "would have the garbage collector delete the archives when the Database
//     is deleted".
//   - So the claims stay spent. Removing a database does not give the
//     namespace its budget back, which is the opposite of what somebody
//     deleting one to make room would assume.
//   - And a database recreated under the same name attaches to the volumes
//     that are still there. That is a feature where it is wanted — the
//     archives survive — and a surprise where it is not: the new database is
//     not empty.
//
// None of that is this endpoint's to change. What it can do is say it, in the
// response, at the moment somebody asks for it.
func deleteDatabase(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ref, ok := g.admit(w, r, authz.ActionDatabaseDelete)
		if !ok {
			return
		}
		name := r.PathValue("database")
		commitChange(w, r, st, sub, ref, "remove database "+name,
			func(_ placement.Placement) renderFunc {
				return func(_ string, current map[string][]byte) (map[string][]byte, error) {
					file := manifest.FileFor("Database", name)
					if _, ok := current[file]; !ok {
						return nil, fmt.Errorf("%w: %q", errNoSuchDatabase, name)
					}
					out := make(map[string][]byte, len(current))
					maps.Copy(out, current)
					delete(out, file)
					if len(out) == 0 {
						// gitwrite refuses an empty render, and it is right to:
						// an app directory with no files is indistinguishable
						// from a render that went wrong. Reported as itself.
						return nil, errLastManifest
					}
					return out, nil
				}
			},
			commitOptions{Note: dataAfterDelete, MayRemove: true})
	})
}

// dataAfterDelete is what the response says, and every clause in it was
// measured in the operator rather than assumed.
const dataAfterDelete = "veritabanı geri çekildi: StatefulSet'i, Service'i ve kimlik " +
	"bilgilerini tutan Secret'ı ona ait, onunla birlikte gidiyor. Veri volume'ü ve yedek " +
	"volume'ü gitmiyor — Kubernetes bir StatefulSet'in claim'lerini bilerek geride bırakır, " +
	"ve arşiv claim'i sahipsiz oluşturulur ki çöp toplayıcı onu alamasın. Yani veri hâlâ " +
	"burada, namespace'in dört claim'inden hâlâ düşüyor, ve bu adla yeniden oluşturulan bir " +
	"veritabanı boş başlamak yerine ona bağlanacak."
