// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/maku/internal/migrategroup"
)

// runMigrateGroup is `maku-broker migrate-group`, the CRD group move's one-shot (the maku rebrand). It
// logs one line per object, then the totals, and fails when any object conflicted or failed. Without
// --apply it writes nothing.
func runMigrateGroup(ctx context.Context, log *slog.Logger, args []string, newClient func() (client.Client, error)) error {
	fs := flag.NewFlagSet("migrate-group", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o migrategroup.Options
	fs.StringVar(&o.From, "from", "", "the API group the objects are under now")
	fs.StringVar(&o.To, "to", "", "the API group to move them to")
	fs.StringVar(&o.Namespace, "namespace", "", "one namespace; every namespace when empty")
	fs.BoolVar(&o.Apply, "apply", false, "write; without it, report what would happen and write nothing")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("maku-broker migrate-group: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("maku-broker migrate-group: unexpected argument %q", fs.Arg(0))
	}
	if err := o.Validate(); err != nil {
		return fmt.Errorf("maku-broker migrate-group: %w", err)
	}
	c, err := newClient()
	if err != nil {
		return fmt.Errorf("maku-broker migrate-group: %w", err)
	}
	rep, err := migrategroup.Run(ctx, c, o)
	if err != nil {
		return fmt.Errorf("maku-broker migrate-group: %w", err)
	}
	for _, r := range rep {
		attrs := []any{"kind", r.Kind, "namespace", r.Namespace, "name", r.Name, "outcome", string(r.Outcome)}
		if r.Err != nil {
			log.ErrorContext(ctx, "migrate-group", append(attrs, "err", r.Err)...)
			continue
		}
		log.InfoContext(ctx, "migrate-group", attrs...)
	}
	log.InfoContext(ctx, "migrate-group done", "from", o.From, "to", o.To, "apply", o.Apply,
		"migrated", rep.Count(migrategroup.Migrated), "resumed", rep.Count(migrategroup.Resumed),
		"wouldCopy", rep.Count(migrategroup.WouldCopy), "wouldResume", rep.Count(migrategroup.WouldResume),
		"conflicts", rep.Count(migrategroup.Conflict), "failed", rep.Count(migrategroup.Failed))
	if !rep.OK() {
		return fmt.Errorf("maku-broker migrate-group: %d conflicts, %d failed (above)",
			rep.Count(migrategroup.Conflict), rep.Count(migrategroup.Failed))
	}
	return nil
}

// kubeClient builds an unstructured-capable client from the kubeconfig kubectl would use:
// KUBECONFIG, else in-cluster, else ~/.kube/config. Never a credential on argv.
func kubeClient(log *slog.Logger) func() (client.Client, error) {
	return func() (client.Client, error) {
		lr := logr.FromSlogHandler(log.Handler())
		ctrl.SetLogger(lr)
		klog.SetLogger(lr)
		cfg, err := ctrl.GetConfig()
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: %w", err)
		}
		return client.New(cfg, client.Options{})
	}
}
