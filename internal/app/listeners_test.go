// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"

	"github.com/Smana/maku/internal/config"
)

// stubListener records its close, so a test can prove an already-opened listener is not leaked.
type stubListener struct{ closed bool }

func (l *stubListener) Accept() (net.Conn, error) { return nil, errors.New("stub: no connection") }
func (l *stubListener) Close() error {
	l.closed = true
	return nil
}
func (l *stubListener) Addr() net.Addr { return &net.TCPAddr{} }

// The listeners the mode and the roomTools block name, and nothing else. The human one follows the
// mode: a local install is reached only through a port-forward, so it binds loopback; an OIDC one
// serves :8080 to oauth2-proxy. The room tools one is the config's switch (I8) — :8090 when absent
// or on, never bound at all when off — and ops never moves. The fake refuses :8090 in the disabled
// cases, so a broker that touched an occupied port fails here instead of at start-up.
func TestOpenBrokerListenersRoomTools(t *testing.T) {
	off, on := false, true
	localHuman := config.HumanConfig{Mode: config.HumanModeLocal}
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		want    []string
		wantMCP bool
	}{
		{"oidc, no roomTools block", config.Config{}, []string{":8080", ":8090", ":9090"}, true},
		{"local, no roomTools block", config.Config{Human: localHuman}, []string{"127.0.0.1:8080", ":8090", ":9090"}, true},
		{"oidc, roomTools enabled", config.Config{RoomTools: &config.RoomToolsConfig{Enabled: &on}},
			[]string{":8080", ":8090", ":9090"}, true},
		{"oidc, roomTools disabled", config.Config{RoomTools: &config.RoomToolsConfig{Enabled: &off}},
			[]string{":8080", ":9090"}, false},
		{"local, roomTools disabled", config.Config{Human: localHuman, RoomTools: &config.RoomToolsConfig{Enabled: &off}},
			[]string{"127.0.0.1:8080", ":9090"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var addrs []string
			listen := func(_ context.Context, network, addr string) (net.Listener, error) {
				if network != "tcp" {
					t.Errorf("network %q", network)
				}
				if addr == mcpAddr && !tc.wantMCP {
					return nil, errors.New("address already in use")
				}
				addrs = append(addrs, addr)
				return &stubListener{}, nil
			}
			human, roomTools, ops, err := openBrokerListeners(t.Context(), tc.cfg, tc.cfg.RoomToolsEnabled(), listen)
			if err != nil {
				t.Fatalf("a broker without room tools must not fail start-up on :8090: %v", err)
			}
			if !slices.Equal(addrs, tc.want) {
				t.Fatalf("opened %v, want %v", addrs, tc.want)
			}
			if (roomTools != nil) != tc.wantMCP {
				t.Fatalf("room tools listener %v, want one: %v", roomTools, tc.wantMCP)
			}
			if human == nil || ops == nil {
				t.Fatalf("human %v, ops %v", human, ops)
			}
		})
	}
	t.Run("a later failure closes what was opened", func(t *testing.T) {
		var opened []*stubListener
		fail := errors.New("address in use")
		listen := func(_ context.Context, _, addr string) (net.Listener, error) {
			if addr == opsAddr {
				return nil, fail
			}
			l := &stubListener{}
			opened = append(opened, l)
			return l, nil
		}
		cfg := config.Config{}
		if _, _, _, err := openBrokerListeners(t.Context(), cfg, cfg.RoomToolsEnabled(), listen); !errors.Is(err, fail) {
			t.Fatalf("want the listen error, got %v", err)
		}
		if len(opened) != 2 || !opened[0].closed || !opened[1].closed {
			t.Fatalf("leaked a listener: %+v", opened)
		}
	})
}
