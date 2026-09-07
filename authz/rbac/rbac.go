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

// Package rbac is the free authorizer: owner, member, viewer, and nothing else.
package rbac

import (
	"context"

	"github.com/damgahq/damga/authz"
)

// The three roles the free tier knows. An identity provider will send groups
// this package has never heard of; the answer to those is viewer, not an error.
const (
	roleOwner  = "owner"
	roleMember = "member"
	roleViewer = "viewer"
)

type Authorizer struct{}

func New() *Authorizer { return &Authorizer{} }

func (a *Authorizer) Authorize(
	_ context.Context, s authz.Subject, act authz.Action, t authz.Target,
) (authz.Decision, error) {
	if s.Tenant == "" || s.Tenant != t.Tenant {
		return authz.Decision{Reason: "subject belongs to a different tenant"}, nil
	}
	role := roleViewer
	for _, g := range s.Groups {
		switch g {
		case roleOwner:
			role = roleOwner
		case roleMember:
			if role != roleOwner {
				role = roleMember
			}
		}
	}
	switch act {
	case authz.ActionAppView, authz.ActionEvidenceView:
		return authz.Decision{Allow: true, Reason: role + " may read"}, nil
	case authz.ActionAppDeploy, authz.ActionAppRollback, authz.ActionEnvCreate, authz.ActionEvidenceExport:
		return authz.Decision{Allow: role != roleViewer, Reason: role + " deploy right"}, nil
	case authz.ActionAppRestart, authz.ActionAppScale:
		// Weaker than deploying, and separate from it on purpose. Neither can
		// introduce code: a restart replaces pods with the image that is
		// already committed, and a scale changes how many of them there are.
		// Folding them into app:deploy would mean an install that wants
		// somebody who may keep the lights on without shipping releases has no
		// way to say so — and the action set is closed precisely so that a new
		// endpoint forces this decision instead of borrowing a neighbour's.
		return authz.Decision{Allow: role != roleViewer, Reason: role + " may change how an app runs"}, nil
	case authz.ActionAppSettings:
		// With the deploy right rather than beside scale and restart, and the
		// difference is what the action can express. A scale changes how many
		// copies run; a setting changes what the process talks to — the
		// database it opens, the API it authenticates against — and a
		// build-time one changes what goes into the image. That is a change to
		// what the application does, which is the promise app:deploy makes.
		//
		// It is still its own action and not app:deploy itself, because the two
		// are not the same permission in the other direction: this one cannot
		// introduce code, and an install that wants somebody who may configure
		// an app without shipping a release has a name for it now.
		return authz.Decision{Allow: role != roleViewer, Reason: role + " may configure an app"}, nil
	case authz.ActionAppDelete:
		// Grouped with deploying rather than with administration, and it was
		// tenant:admin for one release because this action did not exist yet.
		// What a delete removes is a row: the manifests stay committed, what is
		// running keeps running, and the deploy history stays readable. There is
		// no reason somebody who may create an app and ship to it may not also
		// unregister one. A viewer still may not.
		return authz.Decision{Allow: role != roleViewer, Reason: role + " may unregister an app"}, nil
	case authz.ActionAppTrigger:
		// Owner only, and it is the secret that decides this rather than the
		// deploy. Connecting a repository hands out a key that lets whoever
		// holds it make this platform build and ship code, from outside, with
		// no session and no account — the webhook path authenticates the
		// payload and not a person. That is a trust relationship rather than
		// an action, and it is closer to choosing who may deploy than to
		// deploying.
		return authz.Decision{Allow: role == roleOwner, Reason: role + " may connect a repository"}, nil
	case authz.ActionDatabaseCreate:
		// The deploy right, and named here so the reasoning is on the record
		// rather than inferred from the default. Creating a database commits a
		// manifest and takes a persistent volume claim — and a member can
		// already do both, because a Workload carries Volumes and deploying one
		// is the deploy right. Putting this above that line would mean somebody
		// who may ship an application that writes to disk may not ask for the
		// database it writes to, which is a distinction with nothing behind it.
		return authz.Decision{Allow: role != roleViewer, Reason: role + " may create a database"}, nil
	case authz.ActionDatabaseDelete:
		// Owner only, and not grouped with app:delete, which is the one it
		// looks like. Unregistering an application removes a row: the manifests
		// stay committed, what is running keeps running, and the history stays
		// readable. Removing a database withdraws the manifest that keeps a
		// StatefulSet alive, and what happens to the volume after that is not
		// something this control plane can undo — see deleteDatabase, which
		// says so in the response rather than leaving it to be discovered.
		return authz.Decision{Allow: role == roleOwner, Reason: role + " may remove a database"}, nil
	case authz.ActionAppExec:
		// Owner only, and the strongest thing in this set. Everything else here
		// is scoped by what it can express: a deploy ships an image somebody
		// built, a rollback ships one already shipped, a scale changes a
		// number. Running a command inside a container is scoped by nothing —
		// it reads the environment the app was given, which is where its
		// database password is, and it writes wherever that password reaches.
		//
		// It is also the one action that leaves the write path. Principle 1
		// says the only way to change what runs is a commit; an exec changes
		// what a running container has done without changing what it is, so the
		// next sync puts the container back and keeps whatever the command did
		// to the data. Grouping it with the deploy right would hand that to
		// everybody who may ship a release, which is a different and much
		// larger promise than the one they were given.
		return authz.Decision{Allow: role == roleOwner, Reason: role + " may run a command in a container"}, nil
	case authz.ActionAppConnect:
		// Owner only, and named here rather than left to the default, because
		// the reason is not obvious from the verb. Deploying ships an image;
		// connecting decides which signer is trusted for every image this app
		// will ever run. Somebody who can do the second can arrange to be the
		// one who does the first, so grouping it with the deploy right would
		// make the signature check a formality for anybody who could already
		// deploy.
		return authz.Decision{Allow: role == roleOwner, Reason: role + " may choose the signer"}, nil
	default:
		return authz.Decision{Allow: role == roleOwner, Reason: role + " administrative right"}, nil
	}
}
