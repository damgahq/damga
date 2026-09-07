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
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// GitAuth answers how to authenticate to one repository.
//
// Exported because it is a seam another build can replace, and a seam nothing
// outside this package can implement is not a seam. One token for one
// organisation is the whole free story; a GitHub App minting short-lived
// installation tokens per repository, or per-tenant credentials, is a
// different one. Everything above this asks the same question either way.
type GitAuth interface {
	For(repoURL string) (transport.AuthMethod, error)
}

// tokenAuth is the free build's answer: one token, for the organisation damga
// owns the state repositories in.
//
// One credential and not one per tenant, because the plan's arrangement is
// that damga owns the repositories and the tenant has no push identity for
// them. There is nothing per-tenant to hold.
type tokenAuth struct{ token string }

// For returns HTTP basic auth carrying the token.
//
// The username is a constant. Forges that take a token this way ignore it —
// GitHub documents x-access-token, GitLab documents oauth2 — and sending an
// empty one makes go-git omit the header entirely, which fails as a 401 that
// looks like a bad token rather than a missing one.
func (a tokenAuth) For(repoURL string) (transport.AuthMethod, error) {
	if !strings.HasPrefix(repoURL, "https://") {
		// SSH would need a key and a known_hosts policy, and getting the
		// second one wrong is how a platform ends up trusting whatever host
		// answers. Refused rather than half-supported.
		return nil, fmt.Errorf("yalnızca https depoları destekleniyor: %s", repoURL)
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: a.token}, nil
}

// noAuth is what an install with no token configured gets: a clear refusal at
// the moment a deploy is attempted, rather than a clone that fails with
// whatever the forge says about anonymous writes.
type noAuth struct{}

func (noAuth) For(string) (transport.AuthMethod, error) {
	return nil, errors.New("hiçbir git kimlik bilgisi tanımlı değil: -git-token-file geçin")
}

// readGitAuth builds the credential from the configured file.
//
// A file and not an environment variable or a flag value. A flag is in the
// process table and in the shell history; an environment variable is in
// /proc/<pid>/environ, in a crash dump, and in `kubectl describe pod`. A file
// is what a mounted Secret is, and it is the only one of the three that can be
// rotated without restarting anything that reads it fresh.
func readGitAuth(path string) (GitAuth, error) {
	if path == "" {
		return noAuth{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the git token: %w", err)
	}
	// Trimmed, because a token file written with a text editor or by `echo`
	// ends with a newline, and a newline in an HTTP header value is rejected
	// by net/http with an error that says nothing about tokens.
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, fmt.Errorf("the git token file %s is empty", path)
	}
	return tokenAuth{token: token}, nil
}
