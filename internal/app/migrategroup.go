// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/maku/internal/migrategroup"
)

// runMigrateGroup is `maku-broker migrate-group`, the CRD group move's one-shot (the maku rebrand). It
// targets the one cluster KUBECONFIG names (D8): an unset KUBECONFIG, a list of files or a file with
// more than one context is refused, never answered with in-cluster config or ~/.kube/config. Its first
// line names that context and API server. It then logs one line per object, then the totals, and fails
// when any object conflicted or failed. Without --apply it writes nothing.
func runMigrateGroup(ctx context.Context, log *slog.Logger, args []string, getenv func(string) string,
	newClient func(*rest.Config) (client.Client, error)) error {
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
	cfg, kctx, err := oneContextConfig(getenv("KUBECONFIG"))
	if err != nil {
		return fmt.Errorf("maku-broker migrate-group: %w", err)
	}
	host := apiHost(cfg.Host)
	log.InfoContext(ctx, "migrate-group target", "context", kctx, "host", host, "apply", o.Apply)
	c, err := newClient(cfg)
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
	log.InfoContext(ctx, "migrate-group done", "context", kctx, "host", host, "from", o.From, "to", o.To, "apply", o.Apply,
		"migrated", rep.Count(migrategroup.Migrated), "resumed", rep.Count(migrategroup.Resumed),
		"wouldCopy", rep.Count(migrategroup.WouldCopy), "wouldResume", rep.Count(migrategroup.WouldResume),
		"conflicts", rep.Count(migrategroup.Conflict), "failed", rep.Count(migrategroup.Failed))
	if !rep.OK() {
		return fmt.Errorf("maku-broker migrate-group: %d conflicts, %d failed (above)",
			rep.Count(migrategroup.Conflict), rep.Count(migrategroup.Failed))
	}
	return nil
}

// oneContextConfig loads the single kubeconfig file path names and its single context.
func oneContextConfig(path string) (*rest.Config, string, error) {
	if path == "" {
		return nil, "", errors.New("KUBECONFIG is not set: name the one-context kubeconfig of the cluster to migrate")
	}
	if filepath.SplitList(path)[0] != path {
		return nil, "", errors.New("KUBECONFIG names more than one file: name one one-context kubeconfig")
	}
	raw, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig: %w", err)
	}
	if len(raw.Contexts) != 1 {
		return nil, "", fmt.Errorf("kubeconfig %s has %d contexts: migrate-group needs exactly one", path, len(raw.Contexts))
	}
	var name string
	for k := range raw.Contexts {
		name = k
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, name, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig context %s: %w", name, err)
	}
	return cfg, name, nil
}

// apiHost is the API server's host[:port], never its path or query.
func apiHost(server string) string {
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		return u.Host
	}
	return server
}

// kubeClient builds an unstructured-capable client for cfg, routing client-go's logs to log.
func kubeClient(log *slog.Logger) func(*rest.Config) (client.Client, error) {
	return func(cfg *rest.Config) (client.Client, error) {
		lr := logr.FromSlogHandler(log.Handler())
		ctrl.SetLogger(lr)
		klog.SetLogger(lr)
		return client.New(cfg, client.Options{})
	}
}
