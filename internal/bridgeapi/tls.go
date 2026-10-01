// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The :8443 listener's bounds (Ruling AC).
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 16 << 10
)

// TLSConfig serves the pair at certFile and keyFile (GP-18: cert-manager's
// Certificate room-broker-tls). Each handshake compares both files' size and
// modification time with the last seen, and re-reads the pair once either
// changes, so a renewal needs no restart. A pair that fails to load, such as one
// caught mid-rotation, is logged and the last good one stays in service. It
// refuses to start without a loadable pair.
func TLSConfig(certFile, keyFile string, log *slog.Logger) (*tls.Config, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	kp := &keyPair{certFile: filepath.Clean(certFile), keyFile: filepath.Clean(keyFile), log: log}
	stamp, err := kp.stat()
	if err != nil {
		return nil, fmt.Errorf("bridgeapi: the :8443 key pair: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(kp.certFile, kp.keyFile)
	if err != nil {
		return nil, fmt.Errorf("bridgeapi: the :8443 key pair: %w", err)
	}
	kp.cert, kp.seen = &cert, stamp
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: kp.get}, nil
}

// fileStamp is what a rewrite of a file changes. A projected Secret volume swaps a
// symlink; os.Stat follows it to the new file.
type fileStamp struct {
	mod  time.Time
	size int64
}

type keyPair struct {
	certFile, keyFile string
	log               *slog.Logger

	mu   sync.Mutex
	cert *tls.Certificate
	seen [2]fileStamp
}

func (k *keyPair) stat() ([2]fileStamp, error) {
	var out [2]fileStamp
	for i, f := range []string{k.certFile, k.keyFile} {
		fi, err := os.Stat(f)
		if err != nil {
			return out, err
		}
		out[i] = fileStamp{mod: fi.ModTime(), size: fi.Size()}
	}
	return out, nil
}

func (k *keyPair) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	stamp, err := k.stat()
	k.mu.Lock()
	defer k.mu.Unlock()
	if err != nil || stamp == k.seen {
		return k.cert, nil // a file missing mid-rotation: keep the last good pair
	}
	// Remembered even when the load fails: the next attempt waits for the next
	// change instead of re-reading a broken pair on every handshake.
	k.seen = stamp
	cert, err := tls.LoadX509KeyPair(k.certFile, k.keyFile)
	if err != nil {
		k.log.Error("the renewed :8443 key pair does not load; serving the previous one", "err", err)
		return k.cert, nil
	}
	k.cert = &cert
	k.log.Info("reloaded the :8443 key pair", "notAfter", cert.Leaf.NotAfter)
	return k.cert, nil
}

// httpServer is the :8443 listener with its bounds. WriteTimeout is the one
// exception, 0: the SSE stream is a response that never ends, so each
// response gets a write deadline in Routes, which the stream replaces with one per
// write (streamWriteWait), disarmed while the stream is idle.
func (s *Server) httpServer(addr string, cfg *tls.Config) *http.Server {
	srv := &http.Server{Addr: addr, Handler: s.Routes(), TLSConfig: cfg,
		ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, WriteTimeout: 0,
		IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderBytes,
		ErrorLog: slog.NewLogLogger(s.log().Handler(), slog.LevelWarn)}
	srv.RegisterOnShutdown(s.closeStreams)
	return srv
}

// ListenAndServeTLS serves the API on addr with cfg (TLSConfig's) until ctx ends,
// then drains for at most drain. Streams end at once on shutdown: their bridges
// re-dial another replica.
func (s *Server) ListenAndServeTLS(ctx context.Context, addr string, cfg *tls.Config, drain time.Duration) error {
	srv := s.httpServer(addr, cfg)
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServeTLS("", "") }()
	select {
	case err := <-served:
		return fmt.Errorf("bridgeapi: serve %s: %w", addr, err)
	case <-ctx.Done():
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
	defer cancel()
	shut := srv.Shutdown(dctx)
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		shut = errors.Join(shut, err)
	}
	if shut != nil {
		return fmt.Errorf("bridgeapi: shut down %s: %w", addr, shut)
	}
	return nil
}
