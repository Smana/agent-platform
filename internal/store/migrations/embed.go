// SPDX-License-Identifier: Apache-2.0

// Package migrations is the schema, embedded for room-broker migrate (adoption 3b). The same
// files feed cloud-native-ref's Atlas operator through kustomization.yaml.
package migrations

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

// FS holds every migration file, for Files and nothing else to read the directory at run time.
//
//go:embed *.sql
var FS embed.FS

// File is one migration, as room-broker migrate sees it.
type File struct {
	Name    string
	Version string // the timestamp before the first underscore, the key in room_broker_revisions
	Body    string
	// NoTx is Atlas's "-- atlas:txmode none": the file runs a statement at a time, outside a
	// transaction, because CREATE INDEX CONCURRENTLY refuses one.
	NoTx bool
}

// Files lists the embedded migrations in apply order. It lives here, not in store, so its
// Docker-free test is not behind the store package's testcontainers TestMain.
func Files() ([]File, error) {
	names, err := fs.Glob(FS, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]File, 0, len(names))
	for _, name := range names {
		body, err := fs.ReadFile(FS, name)
		if err != nil {
			return nil, err
		}
		out = append(out, File{
			Name:    name,
			Version: strings.SplitN(name, "_", 2)[0],
			Body:    string(body),
			NoTx:    strings.HasPrefix(string(body), "-- atlas:txmode none"),
		})
	}
	return out, nil
}
