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
	"time"

	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/placement"
)

// BackupStatus is what the evidence page can say about an app's database.
//
// Every field is something somebody wrote down after it happened. "Restored
// three hours ago, 1,284 rows verified" is a claim about the past, and nothing
// live can be asked it — which is why this is read from a status rather than
// computed.
type BackupStatus struct {
	// Database is the name the app is connected to, so a page can say which one
	// it is talking about when an app has none and its neighbour does.
	Database string

	// Rehearsed is whether the last run restored the archive or only wrote it.
	// The two are different claims and the weaker one is still worth showing:
	// an install that turned the rehearsal off has backups, and saying so is
	// more honest than a blank line.
	Rehearsed bool

	FinishedAt time.Time
	Rows       int64
	SourceRows int64
	Tables     int32
}

// ErrNoDatabase is an app that is not connected to one. Not an error condition:
// most apps have no database, and a page that renders that as a failure is
// wrong about the ordinary case.
var ErrNoDatabase = errors.New("server: this app has no database")

// BackupReader answers what the platform observed about an app's backups.
//
// A seam for the same reason the stores are, plus one of its own: this is the
// first thing in the server that reads the cluster. Everything else here writes
// to git and reads from a database, and that separation is deliberate — so the
// one place that breaks it is behind an interface, where a build that has no
// cluster to read can leave it out and a build that has a different one can
// replace it.
type BackupReader interface {
	// ForApp resolves the app's database and returns its last rehearsal.
	// It returns ErrNoDatabase when the app names none.
	ForApp(ctx context.Context, namespace, app string) (BackupStatus, error)
}

type wireBackup struct {
	Database string `json:"database"`
	// State is the sentence the page renders, decided here rather than in the
	// panel — the panel decides nothing, and "restored" versus "backed up only"
	// is a decision about what the platform is entitled to claim.
	State      string `json:"state"`
	Rehearsed  bool   `json:"rehearsed"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Rows       int64  `json:"rows"`
	SourceRows int64  `json:"sourceRows"`
	Tables     int32  `json:"tables"`
}

// backupRoute says what became of this app's backups.
//
// Its own endpoint rather than a block on the evidence record, because the
// record is one deploy and a backup is not: attaching it there would put a
// number that changes nightly inside an object whose whole value is that it
// does not change.
func backupRoute(g guard, st stores) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := g.admit(w, r, authz.ActionEvidenceView)
		if !ok {
			return
		}
		if st.backups == nil {
			// An install with no cluster reader is a working install whose page
			// cannot show this line. Said plainly rather than answered with an
			// empty status, which would read as "no backups" — the one thing it
			// must not be mistaken for.
			problem(w, http.StatusNotImplemented,
				"bu kurulum kümeden veritabanı durumunu okuyamıyor")
			return
		}

		// The namespace comes from the placement and not from a convention, for
		// the reason placement carries it as a field: a namespace derived from
		// the tenant and the environment is a name parsed out of an identity.
		place, err := st.placement.Get(r.Context(), ref.TenantID, ref.App, ref.Env)
		switch {
		case errors.Is(err, placement.ErrNotFound):
			problem(w, http.StatusNotFound, "bu uygulama ve ortam için henüz bir depo tanımlı değil")
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "yerleşim kaydı okunamadı")
			return
		}

		got, err := st.backups.ForApp(r.Context(), place.Namespace, ref.App)
		switch {
		case errors.Is(err, ErrNoDatabase):
			// 404 and not an empty body. "This app has no database" and "this
			// app's backups are unknown" are different answers and the page
			// renders them differently.
			problem(w, http.StatusNotFound, "bu uygulama bir veritabanına bağlı değil")
			return
		case err != nil:
			problem(w, http.StatusBadGateway, "veritabanı durumu okunamadı")
			return
		}

		out := wireBackup{
			Database: got.Database, Rehearsed: got.Rehearsed,
			Rows: got.Rows, SourceRows: got.SourceRows, Tables: got.Tables,
		}
		switch {
		case got.FinishedAt.IsZero():
			// A database whose first backup has not run yet. Not a failure and
			// not a success, and the page has to be able to say the third thing.
			out.State = "none yet"
		case got.Rehearsed:
			out.State = "restored"
			out.FinishedAt = got.FinishedAt.UTC().Format(time.RFC3339)
		default:
			out.State = "backed up"
			out.FinishedAt = got.FinishedAt.UTC().Format(time.RFC3339)
		}
		writeJSON(w, map[string]any{"backup": out})
	})
}
