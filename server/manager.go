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
	"fmt"
	"log/slog"
	"net/http"
	"time"

	platformv1alpha1 "github.com/damgahq/damga/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/internal/buildwatch"
	"github.com/damgahq/damga/internal/deploywatch"
	"github.com/damgahq/damga/internal/gitwrite"
)

// managerGracePeriod is how long controller-runtime waits for its runnables.
// The HTTP server's own timeout is held strictly below it, for a reason that is
// measured rather than tidy — see shutdownTimeout below.
const managerGracePeriod = 30 * time.Second

// runWithManager hands the process to controller-runtime.
func (o Options) runWithManager(
	ctx context.Context, srv *http.Server, store evidence.Store, log *slog.Logger,
) error {
	restCfg := o.RestConfig
	if restCfg == nil {
		var err error
		if restCfg, err = ctrl.GetConfig(); err != nil {
			return fmt.Errorf("observing deploys needs a cluster: %w", err)
		}
	}

	grace := managerGracePeriod
	// The platform's own types, on the manager's scheme.
	//
	// deploywatch watches Deployments, which are in the default scheme, so
	// nothing here had ever needed this. buildwatch watches a Build, and a
	// manager whose scheme does not know the kind fails at startup rather than
	// at the first object — measured on a real cluster on 2026-09-07:
	//
	//	damga stopped error="build observer: no kind is registered for the
	//	type v1alpha1.Build in scheme"
	//
	// Which is the right failure and the wrong place to discover it: every
	// unit test passes, because none of them builds a manager.
	mgrScheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(mgrScheme); err != nil {
		return fmt.Errorf("registering the core types: %w", err)
	}
	if err := platformv1alpha1.AddToScheme(mgrScheme); err != nil {
		return fmt.Errorf("registering the platform types: %w", err)
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: mgrScheme,
		// The manager's own metrics endpoint is off. The panel's listener is
		// the one this process is for, and a second one bound by default is how
		// a port collision becomes a startup failure nobody asked for.
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		LeaderElection:          o.Config.LeaderElect,
		LeaderElectionNamespace: o.Config.LeaderElectionNamespace,
		// Not the operator's a73f34a4.damga.co. Two managers sharing a lease
		// name means one of them silently never runs its controllers, and the
		// symptom is an evidence page that stops updating rather than an error.
		LeaderElectionID:        "server.damga.co",
		GracefulShutdownTimeout: &grace,
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	// Handed over as the concrete *manager.Server, which matters more than it
	// looks: controller-runtime's runnable group has a dedicated branch for
	// that type that puts it in the HTTP group, started before anything else
	// and before leadership is even attempted. Wrapped in a RunnableFunc it
	// would fall through to the generic group instead and only start after the
	// caches sync — so an unreachable API server would take the panel down with
	// it, which is precisely the coupling this shape exists to avoid.
	sd := o.shutdownTimeout()
	if err := mgr.Add(&manager.Server{
		Name:                "panel",
		Server:              srv,
		ShutdownTimeout:     &sd,
		OnlyServeWhenLeader: false,
	}); err != nil {
		return fmt.Errorf("adding the panel server: %w", err)
	}

	if err := (&deploywatch.Reconciler{
		Client:   mgr.GetClient(),
		Evidence: store,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("deploy observer: %w", err)
	}

	// The third link of `git push -> build -> deploy`, and the one that was
	// missing entirely: build_controller wrote a digest into a status field
	// and nothing read it. This reads it and asks for a commit — it does not
	// touch the cluster's desired state, because a build stands in front of
	// the write path and not inside it.
	//
	// Registered only when there is somewhere to write. An installation with
	// no git credential can still start, serve and build; what it cannot do is
	// turn a build into a deploy, and a reconciler that failed on every
	// succeeded build would say that once a minute for ever.
	if o.Placement != nil && o.GitAuth != nil {
		if err := (&buildwatch.Reconciler{
			Client: mgr.GetClient(),
			Deployer: buildDeployer{
				places: o.Placement,
				// The same writer the endpoints use, constructed the same way.
				// It carries no state beyond the evidence store, so a second
				// value is the same writer rather than a second one.
				writer:  &gitwrite.Writer{Evidence: store},
				gitAuth: o.GitAuth,
			},
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("build observer: %w", err)
		}
	}

	// A Runnable rather than a controller, because it is driven by a clock and
	// not by an object. It declares NeedLeaderElection, so it lands in the
	// leader-election group beside the reconciler.
	if err := mgr.Add(&deploywatch.Sweep{
		Evidence: store,
		After:    o.Config.PendingTimeout,
		Every:    time.Minute,
	}); err != nil {
		return fmt.Errorf("evidence sweep: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	log.Info("damga is listening", "address", o.Config.ListenAddr, "observing", true)
	if o.Ready != nil {
		// The manager binds the listener itself, so the address reported here
		// is the configured one rather than a resolved one. A caller that asked
		// for port zero and is observing gets ":0" back, which is useless — and
		// is why the tests that need a real address run without the manager.
		o.Ready(o.Config.ListenAddr)
	}
	return mgr.Start(ctx)
}

// shutdownTimeout keeps the HTTP server's grace period strictly below the
// manager's.
//
// controller-runtime's Start returns nil when the context is cancelled and then
// its deferred stop procedure overwrites that with "failed waiting for all
// runnables to end within grace period". A manager.Server with no timeout of
// its own passes context.Background() to http.Server.Shutdown, which waits for
// ever — so a single in-flight request turns every clean shutdown into an
// error. Measured: unbounded, one in-flight request produced a non-nil error
// after 30s; bounded below the grace period, nil.
func (o Options) shutdownTimeout() time.Duration {
	sd := o.Config.ShutdownTimeout
	if sd <= 0 {
		sd = 15 * time.Second
	}
	if max := managerGracePeriod - 5*time.Second; sd > max {
		sd = max
	}
	return sd
}

// clusterReader builds an uncached client for the one thing this server reads
// from the cluster.
//
// Its own client rather than the manager's, and the reason is ordering: the
// handler is built before the manager exists, and the manager's client reads a
// cache that is empty until it starts. A reader taken from there would report
// every app as having no database until something happened to warm it.
func (o Options) clusterReader() (client.Reader, error) {
	restCfg := o.RestConfig
	if restCfg == nil {
		var err error
		if restCfg, err = ctrl.GetConfig(); err != nil {
			return nil, fmt.Errorf("reading database status needs a cluster: %w", err)
		}
	}
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("scheme: %w", err)
	}
	return client.New(restCfg, client.Options{Scheme: scheme})
}
