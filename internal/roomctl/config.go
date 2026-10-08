// SPDX-License-Identifier: Apache-2.0

// Package roomctl is the human CLI's client (SP2 §8, phase 6): it lists rooms,
// follows one, posts or queues a message, and forks. It never steers,
// interrupts, moves the driver token or decides: a local agent can drive a
// terminal (ruling P18), and the broker refuses those acts from this client's
// tokens anyway.
package roomctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// Config is what `roomctl configure` saves; the room list's CLI setup view
// (GET /api/roomctl) shows every value.
type Config struct {
	URL       string `json:"url"`       // the rooms host, https://rooms.<private domain>
	Issuer    string `json:"issuer"`    // ZITADEL
	ClientID  string `json:"clientID"`  // the roomctl native app
	ProjectID string `json:"projectID"` // a token's aud must hold it, and ZITADEL adds it only when asked
}

// Validate refuses a plain-http URL, which would carry the bearer token in clear,
// and a missing id.
func (c Config) Validate() error {
	for name, raw := range map[string]string{"url": c.URL, "issuer": c.Issuer} {
		if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("%s %q is not an https URL", name, raw)
		}
	}
	if c.ClientID == "" || c.ProjectID == "" {
		return errors.New("the client id and the project id are both required")
	}
	return nil
}

// Dir is where roomctl keeps its config and token: <user config dir>/roomctl.
func Dir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "roomctl"), nil
}

// SaveJSON writes v owner-only, through a new file renamed into place: the token
// file holds a refresh token, and WriteFile would keep an existing file's mode.
func SaveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".roomctl-*") // 0600
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() // a no-op once renamed
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// LoadJSON reads what SaveJSON wrote.
func LoadJSON(path string, v any) error {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
