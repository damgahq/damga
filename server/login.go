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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/damgahq/damga/auth"
	"github.com/damgahq/damga/authz"
	"github.com/damgahq/damga/identity"
)

// The one message every failed login gets.
//
// A handler that says "no such account" for one address and "wrong password"
// for another is an account enumeration oracle with a user interface. The
// timing is handled separately, by hashing a dummy when there is no account —
// saying the same thing while answering faster leaks exactly as much.
const loginRefused = "bu e-posta ve parola bir hesapla eşleşmiyor"

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// login exchanges an email and a password for a session cookie.
func (o Options) login(idStore identity.Store, sess *auth.Sessions, hasher *auth.Hasher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "istek gövdesi beklenen JSON değil")
			return
		}
		if req.Email == "" || req.Password == "" {
			problem(w, http.StatusUnauthorized, loginRefused)
			return
		}

		account, err := idStore.AccountByEmail(r.Context(), req.Email)
		switch {
		case errors.Is(err, identity.ErrNotFound):
			// The same work as a real verification, so an address that does not
			// exist does not answer sooner than one that does.
			hasher.VerifyDummy(req.Password)
			problem(w, http.StatusUnauthorized, loginRefused)
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "kimlik deposu kullanılamıyor")
			return
		}

		cred, err := idStore.Credential(r.Context(), account.ID)
		switch {
		case errors.Is(err, identity.ErrNotFound):
			// A federated account has no password. Not an error and not a
			// different answer: telling the caller "this account uses SSO"
			// confirms the address exists.
			hasher.VerifyDummy(req.Password)
			problem(w, http.StatusUnauthorized, loginRefused)
			return
		case err != nil:
			problem(w, http.StatusInternalServerError, "kimlik deposu kullanılamıyor")
			return
		}

		if err := hasher.Verify(cred.Hash, req.Password); err != nil {
			if !errors.Is(err, auth.ErrMismatch) {
				// A hash that cannot be read is a data problem, and reporting
				// it as a wrong password would leave the account permanently
				// unable to log in with nobody able to say why.
				slog.Default().Error("stored credential is unreadable",
					"account", account.ID, "error", err)
			}
			problem(w, http.StatusUnauthorized, loginRefused)
			return
		}

		// A disabled account is checked after the password on purpose: checking
		// first would answer instantly for disabled accounts and turn the
		// disabled flag into an oracle of its own.
		if account.Disabled {
			problem(w, http.StatusUnauthorized, loginRefused)
			return
		}

		cookie, err := sess.Issue(r.Context(), account.ID, r.Host)
		if err != nil {
			problem(w, http.StatusInternalServerError, "oturum oluşturulamadı")
			return
		}
		http.SetCookie(w, cookie)

		// Raise an old hash while the plaintext is in hand — this is the only
		// moment it exists. RehashCredential rather than SetCredential, because
		// the second revokes every session and would log the user out of the
		// one this request has just issued.
		//
		// A failure is logged and not surfaced: the login succeeded, and the
		// password is no weaker than it was a moment ago.
		if hasher.NeedsRehash(cred.Hash) {
			fresh, hErr := hasher.Hash(req.Password)
			if hErr == nil {
				hErr = idStore.RehashCredential(r.Context(), identity.Credential{
					AccountID: account.ID, Hash: fresh,
				})
			}
			if hErr != nil {
				slog.Default().Warn("could not raise a stored password hash",
					"account", account.ID, "error", hErr)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account": map[string]string{
				"id": account.ID, "email": account.Email, "displayName": account.DisplayName,
			},
		})
	})
}

// logout revokes the session and clears the cookie.
func (Options) logout(sess *auth.Sessions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, sess.Revoke(r.Context(), r))
		w.WriteHeader(http.StatusNoContent)
	})
}

// subjectFrom builds the authorizer's subject from the session and the
// membership row, and from nothing else.
//
// This is the function the placeholder headers existed to stand in for, and the
// reason it takes a tenant rather than reading one from the request: the free
// authorizer treats an unrecognised group as viewer, and a viewer may read the
// evidence page — so a subject assembled from anything the caller sent would
// let a stranger read another tenant's deploy history. No membership means no
// subject, never a viewer.
func subjectFrom(
	ctx context.Context, idStore identity.Store, sess identity.Session, tenantID string,
) (authz.Subject, error) {
	account, err := idStore.Account(ctx, sess.AccountID)
	if err != nil {
		return authz.Subject{}, err
	}
	if account.Disabled {
		return authz.Subject{}, identity.ErrNotFound
	}
	m, err := idStore.Membership(ctx, sess.AccountID, tenantID)
	if err != nil {
		return authz.Subject{}, err
	}

	// The tenant row is read for two things that cannot come from anywhere
	// else: whether it is suspended, and which plan it is on.
	tenant, err := idStore.Tenant(ctx, m.TenantID)
	if err != nil {
		return authz.Subject{}, err
	}
	if tenant.Suspended {
		// A suspended tenant is refused here rather than by each authorizer.
		// Suspension is a fact about the tenant, not a policy decision, so
		// leaving it to the authorizer would mean the free one and a paid one
		// could disagree about whether an unpaid account can still deploy.
		return authz.Subject{}, identity.ErrNotFound
	}

	return authz.Subject{
		ID:     account.ID,
		Tenant: m.TenantID,
		Email:  account.Email,
		Groups: []string{string(m.Role)},
		// From the row, never from the request. A tier a caller could set is
		// a licence check with a query parameter for a bypass.
	}, nil
}
