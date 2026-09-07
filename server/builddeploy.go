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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/internal/gitwrite"
	"github.com/damgahq/damga/internal/manifest"
	"github.com/damgahq/damga/placement"
)

// buildDeployer turns a finished build into a commit.
//
// The server side of internal/buildwatch's seam. It is here rather than there
// because everything it needs — the placement store, the git credential, the
// renderer that produces a Workload — is the server's, and moving any of it
// would be a second definition of what a deploy is.
type buildDeployer struct {
	places  placement.Store
	writer  *gitwrite.Writer
	gitAuth GitAuth
}

// Deploy writes image as the environment's image, and says whether it wrote.
//
// The repetition check is here rather than in the caller, and it is not an
// optimisation. buildwatch cannot mark a Build as handled — its ClusterRole
// holds no patch on builds, deliberately — so on restart it will offer the
// same succeeded build again. gitwrite cannot refuse the repeat either:
// manifest.Render stamps a fresh rollout id every time, so the rendered bytes
// always differ and ErrNoChange cannot fire. Without a comparison against what
// is committed, a restart would commit the same digest again, and every
// restart after that once more.
//
// So this reads the committed manifest first. It costs one clone, which is the
// price of the guarantee, and it is only paid for a build that succeeded.
func (d buildDeployer) Deploy(ctx context.Context, tenantID, app, env, image string) (bool, error) {
	place, err := d.places.Get(ctx, tenantID, app, env)
	if err != nil {
		if errors.Is(err, placement.ErrNotFound) {
			// Not retried into a loop: an app whose placement was removed
			// while a build was running has nowhere to deploy, and that is an
			// answer rather than a failure.
			return false, nil
		}
		return false, fmt.Errorf("reading the placement: %w", err)
	}

	method, err := d.gitAuth.For(place.RepoURL)
	if err != nil {
		return false, fmt.Errorf("the git credential: %w", err)
	}
	target := gitwrite.Target{
		RepoURL: place.RepoURL, Branch: place.Branch, Dir: place.Path, Auth: method,
	}

	committed, err := d.writer.Read(ctx, target)
	if err != nil {
		return false, fmt.Errorf("reading what is committed: %w", err)
	}
	if body, ok := committed[manifest.File]; ok {
		current, err := manifest.Parse(body)
		if err == nil && current.Spec.Image == image {
			return false, nil
		}
	}

	_, err = d.writer.Deploy(ctx, gitwrite.Request{
		Target: target,
		// The platform is the author, and it says so. A build is not a person
		// asking for a deploy — it is the consequence of a push somebody else
		// made, and attributing the commit to a user who never pressed
		// anything would put a name on the evidence page that did not act.
		Author: gitwrite.Author{
			ID: "automation", Name: managedByDamga, Email: "damga@users.damga.local",
		},
		Ref:     evidence.Ref{TenantID: tenantID, App: app, Env: env},
		Image:   image,
		Message: "deploy " + app + "/" + env + " from a build",
		Render: func(rolloutID string, current map[string][]byte) (map[string][]byte, error) {
			return renderDeploy(place, deployRequest{Image: image})(rolloutID, current)
		},
	})
	if err != nil {
		return false, fmt.Errorf("committing the deploy: %w", err)
	}
	return true, nil
}

// inClusterBuilds is the BuildCreator every installation was missing.
//
// server.Options declares Builds and says it is "filled by the composition
// root". Nothing filled it: `o.Builds` is read in exactly one place and
// assigned in none, in any binary, so createBuild and the webhook path both
// answered 501 in every build of this product. The endpoint existed, the
// webhook verified signatures and resolved triggers, and the seam behind them
// was nil.
//
// That is the same shape as the rest of this change and one joint earlier than
// the audit that started it: `POST /apps/{app}/builds` was true of the handler
// and not of the product.
//
// Resolved from the pod's own ServiceAccount at first use rather than passed
// in, which is what logs and exec already do, and it is what makes the
// ordering work: the HTTP handlers are constructed before the manager exists,
// so there is no client to hand them at that point.
func inClusterBuilds() (BuildCreator, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("starting builds needs the cluster this pod is in: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering the platform types: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building the cluster client: %w", err)
	}
	return clusterBuilds{c: c}, nil
}

// clusterBuilds creates Builds through the Kubernetes API.
type clusterBuilds struct{ c client.Client }

func (b clusterBuilds) CreateBuild(ctx context.Context, build *platformv1alpha1.Build) error {
	return b.c.Create(ctx, build)
}

// NewClusterBuilds builds a BuildCreator over a client, so something outside
// this package can fill the seam.
func NewClusterBuilds(c client.Client) BuildCreator { return clusterBuilds{c: c} }

// lazyBuilds defers resolving the cluster until the first build is asked for.
//
// A failure is reported as the 501 createBuild already gives, with its own
// sentence, rather than as a startup failure: an installation outside a
// cluster must still start, serve and say what it cannot do.
type lazyBuilds struct{ open func() (BuildCreator, error) }

func (l lazyBuilds) CreateBuild(ctx context.Context, build *platformv1alpha1.Build) error {
	creator, err := l.open()
	if err != nil {
		return fmt.Errorf("%w: %w", errBuildsUnavailable, err)
	}
	return creator.CreateBuild(ctx, build)
}

// errBuildsUnavailable marks the case where there is no cluster to build in, so
// createBuild can tell it apart from a build the cluster refused.
var errBuildsUnavailable = errors.New("this installation cannot start builds")
