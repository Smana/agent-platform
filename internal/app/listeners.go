// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Smana/agent-platform/internal/config"
)

// ListenFunc opens one listener. Production passes net.ListenConfig.Listen; tests pass a fake that
// records the addresses, so the chosen ones are asserted without binding a port.
type ListenFunc func(ctx context.Context, network, addr string) (net.Listener, error)

// openBrokerListeners opens the broker's plain-HTTP listeners, in start-up order, and closes the ones
// already opened if a later one fails: a refused port must not leave a listener behind. The human
// listener is the one that follows the mode — a local install is reached only through a port-forward,
// so it binds loopback, while an OIDC one serves :8080 to oauth2-proxy. The room tools (:8090) and ops
// (:9090) listeners do not move.
//
// roomToolsEnabled false opens no :8090 at all and returns a nil roomTools. Binding it and closing it
// again would fail start-up on a port something else holds, for a broker that answers nothing there:
// the caller passes config.Config.RoomToolsEnabled(), which is off only when the file says so.
func openBrokerListeners(ctx context.Context, cfg config.Config, roomToolsEnabled bool, listen ListenFunc) (human, roomTools, ops net.Listener, err error) {
	opened := make([]net.Listener, 0, 3)
	open := func(name, at string) (net.Listener, error) {
		ln, lerr := listen(ctx, "tcp", at)
		if lerr != nil {
			return nil, errors.Join(fmt.Errorf("room-broker: %s listener: %w", name, lerr), closeListeners(opened...))
		}
		opened = append(opened, ln)
		return ln, nil
	}
	if human, err = open("human", humanListenAddr(cfg)); err != nil {
		return nil, nil, nil, err
	}
	if roomToolsEnabled {
		if roomTools, err = open("room tools", mcpAddr); err != nil {
			return nil, nil, nil, err
		}
	}
	if ops, err = open("ops", opsAddr); err != nil {
		return nil, nil, nil, err
	}
	return human, roomTools, ops, nil
}

// closeListeners closes what start-up opened, so a later failure leaves no port bound.
func closeListeners(lns ...net.Listener) error {
	var errs []error
	for _, ln := range lns {
		if ln != nil {
			errs = append(errs, ln.Close())
		}
	}
	return errors.Join(errs...)
}
