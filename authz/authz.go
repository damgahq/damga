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

// Package authz is the single place a permission is decided. Principle 6 is
// enforced by there being exactly one method here: if a new question needs a
// new method, the principle has already been broken.
package authz

import "context"

// Action is a verb the API offers. The set is closed and lives here, so that
// adding an endpoint forces a decision about who may call it.
type Action string

const (
	ActionAppView        Action = "app:view"
	ActionAppDeploy      Action = "app:deploy"
	ActionAppRollback    Action = "app:rollback"
	ActionAppRestart     Action = "app:restart"
	ActionAppScale       Action = "app:scale"
	ActionAppSettings    Action = "app:settings"
	ActionAppDelete      Action = "app:delete"
	ActionAppConnect     Action = "app:connect"
	ActionAppExec        Action = "app:exec"
	ActionDatabaseCreate Action = "database:create"
	ActionDatabaseDelete Action = "database:delete"
	ActionAppTrigger     Action = "app:trigger"
	ActionEnvCreate      Action = "env:create"
	ActionEvidenceView   Action = "evidence:view"
	ActionEvidenceExport Action = "evidence:export"
	ActionMemberInvite   Action = "member:invite"
	ActionTenantAdmin    Action = "tenant:admin"
)

// Subject is who is asking. Groups is where the free roles (owner, member,
// viewer) and an SSO provider's claims meet: the free authorizer reads the
// three it knows, damga-ee's reads whatever the IdP sent.
type Subject struct {
	ID     string
	Tenant string
	Email  string
	Groups []string

	// Attributes carries claims this package has no opinion about. The
	// default authorizer ignores it; a finer one need not.
	Attributes map[string]string
}

// Target is what is being acted on. Empty App or Env means the question is
// about the tenant as a whole.
type Target struct {
	Tenant string
	App    string
	Env    string
}

// Decision is the answer. Reason is not decoration: it is shown to the user
// and copied into the evidence record, so a refusal is explainable months
// later.
type Decision struct {
	Allow  bool
	Reason string
}

// Authorizer is the seam. One method, so damga-ee implements one function.
type Authorizer interface {
	Authorize(ctx context.Context, s Subject, a Action, t Target) (Decision, error)
}

// CanDeploy is the plan's canUserDeploy(user, app, env), spelled as a helper
// over the one interface rather than as a second method on it.
func CanDeploy(ctx context.Context, a Authorizer, s Subject, t Target) (Decision, error) {
	return a.Authorize(ctx, s, ActionAppDeploy, t)
}
