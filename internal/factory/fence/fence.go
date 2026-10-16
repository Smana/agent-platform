// SPDX-License-Identifier: Apache-2.0

// Package fence is the single-owner fence of the v2 second-repository design (D7): a repository is
// acted on only by the factory its default branch names, in `.github/agent-factory.yaml`
// (`factory: <name>`). Absent, unreadable, or naming another factory, the repository is not this
// factory's: no intake, no schedules, no API runs, no merges. Two clusters reading the same file
// can never both own it, whatever their pins say.
package fence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"
)

// Path is the fence file, read on each repository's default branch.
const Path = ".github/agent-factory.yaml"

// nameRE is a factory name: a DNS label, as cluster names are.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidName reports whether name can be a factory's name in the fence file.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Parse reads a fence file strictly: exactly `factory: <name>`. Anything else is an error, and the
// caller treats the repository as not owned.
func Parse(raw []byte) (string, error) {
	j, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return "", errors.New("fence: not YAML")
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	var f struct {
		Factory string `json:"factory"`
	}
	if err := dec.Decode(&f); err != nil {
		return "", errors.New("fence: not {factory: <name>}")
	}
	if !ValidName(f.Factory) {
		return "", fmt.Errorf("fence: %q is not a factory name", f.Factory)
	}
	return f.Factory, nil
}

// Fence knows which listed repositories name this factory. Every replica runs one: the API serves
// on all of them.
type Fence struct {
	// Name is this factory's name, the one a fence file must give.
	Name string
	// Read is the fence file on repository's default branch; found is false when GitHub has none.
	Read         func(ctx context.Context, repository string) (body []byte, found bool, err error)
	Repositories []string
	// Every is the read period (5 minutes in the factory). Stale bounds how long a failed read
	// keeps the last answer: past it, a repository is not owned until a read succeeds.
	Every, Stale time.Duration
	Now          func() time.Time
	// OnState hears each repository's state after every refresh (agent_factory_repository_owned).
	OnState func(ctx context.Context, repository string, owned bool)
	Log     *slog.Logger

	mu    sync.RWMutex
	state map[string]entry
}

type entry struct {
	owned bool
	at    time.Time
}

// Owned reports whether repository's fence file names this factory, as last read within Stale.
// A repository never read, or not listed, is not owned.
func (f *Fence) Owned(repository string) bool {
	f.mu.RLock()
	e, ok := f.state[strings.ToLower(repository)]
	f.mu.RUnlock()
	return ok && e.owned && f.Now().Sub(e.at) < f.Stale
}

// Refresh reads every listed repository's fence file once. A read that fails leaves that
// repository's last answer in place, to age out at Stale; the errors are returned together.
func (f *Fence) Refresh(ctx context.Context) error {
	var errs []error
	for _, repo := range f.Repositories {
		body, found, err := f.Read(ctx, repo)
		if err != nil {
			errs = append(errs, fmt.Errorf("fence %s: %w", repo, err))
		} else {
			owner := ""
			if found {
				if owner, err = Parse(body); err != nil {
					f.log().Warn("fence file unreadable: the repository is not this factory's", "repository", repo, "err", err)
				}
			}
			f.mu.Lock()
			if f.state == nil {
				f.state = map[string]entry{}
			}
			f.state[strings.ToLower(repo)] = entry{owned: owner == f.Name, at: f.Now()}
			f.mu.Unlock()
		}
		if f.OnState != nil {
			f.OnState(ctx, repo, f.Owned(repo))
		}
	}
	return errors.Join(errs...)
}

// NeedLeaderElection is false: every replica's API asks it.
func (*Fence) NeedLeaderElection() bool { return false }

// Start refreshes at once, then every Every, until ctx ends.
func (f *Fence) Start(ctx context.Context) error {
	if f.Every <= 0 {
		return fmt.Errorf("fence: the read period %s is not positive", f.Every)
	}
	t := time.NewTicker(f.Every)
	defer t.Stop()
	for {
		if err := f.Refresh(ctx); err != nil {
			f.log().Warn("fence read failed: the last answer stands until it is stale", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (f *Fence) log() *slog.Logger {
	if f.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return f.Log
}
