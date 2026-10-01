// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	// brokerTimeout bounds one hello or batch, inside the broker's own 30 s route bound.
	brokerTimeout = 15 * time.Second
	// maxReplyBytes bounds a hello, ack or refusal body.
	maxReplyBytes = 64 << 10
	// streamIdle ends a stream that sent nothing, not even the broker's 30 s
	// ping, for this long: a half-open connection would otherwise hang it.
	streamIdle = 75 * time.Second
	// maxFrameBytes bounds one SSE line, and one event's data lines together
	// (review M15): a deliver carries at most a 16 KiB human message, escaped.
	maxFrameBytes = 256 << 10
)

// Reply is the broker's answer to a request that reached it. Reason is the
// wire.Error reason of a refusal; RetryAfter is the raw Retry-After header.
type Reply struct {
	Code       int
	Reason     string
	RetryAfter string
}

// Broker is the bridge's client of the broker's :8443, over TLS trusting only
// the broker's CA (GP-18). The token is re-read before every request and never
// watched: under gVisor, kubelet's host-side rotation raises no inotify, but a
// plain read sees the new file (SP1 spike Q2).
type Broker struct {
	base      string
	tokenFile string
	hc        *http.Client
	stream    *http.Client
}

// NewBroker returns the client of the broker at base, which must be https://.
// caFile holds the PEM CA the broker's certificate chains to, and is the only
// root trusted. It refuses to start without one.
func NewBroker(base, tokenFile, caFile string) (*Broker, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("bridge: the broker URL must be https://host[:port], got %q", base)
	}
	pemBytes, err := os.ReadFile(filepath.Clean(caFile))
	if err != nil {
		return nil, fmt.Errorf("bridge: the broker CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("bridge: the broker CA %s holds no PEM certificate", caFile)
	}
	stream := httpx.New(brokerTimeout, roots)
	// The stream lives up to an hour: the context and streamIdle bound it, not
	// a whole-exchange timeout. Headers are still bounded by the transport.
	stream.Timeout = 0
	return &Broker{base: strings.TrimRight(base, "/"), tokenFile: filepath.Clean(tokenFile),
		hc: httpx.New(brokerTimeout, roots), stream: stream}, nil
}

func (b *Broker) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	tok, err := os.ReadFile(b.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read the room token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("broker %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// call sends body and decodes a 200's JSON into out. A refusal is a Reply, not
// an error; an error means the request did not get an answer.
func (b *Broker) call(ctx context.Context, path string, body []byte, out any) (Reply, error) {
	req, err := b.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return Reply{}, err
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("broker POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	rep := Reply{Code: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	raw, err := httpx.ReadBody(resp.Body, maxReplyBytes)
	if err != nil {
		return rep, fmt.Errorf("broker POST %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e wire.Error
		if json.Unmarshal(raw, &e) == nil {
			rep.Reason = e.Reason
		}
		return rep, nil
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return rep, fmt.Errorf("broker POST %s: %w", path, err)
		}
	}
	return rep, nil
}

// Hello claims the room's bridge lease and reads where the log already is.
func (b *Broker) Hello(ctx context.Context) (wire.Resume, Reply, error) {
	var r wire.Resume
	rep, err := b.call(ctx, "/v1/bridge/hello", nil, &r)
	return r, rep, err
}

// Send posts one batch of items, each already a marshalled wire.Item, so the
// bytes the caller measured are the bytes sent.
func (b *Broker) Send(ctx context.Context, items []json.RawMessage) (Reply, error) {
	return b.call(ctx, "/v1/bridge/events", joinBatch(items), nil)
}

// joinBatch is json.Marshal(wire.Batch{Items: items}) for pre-marshalled items.
func joinBatch(items []json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"items":[`)
	for i, it := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(it)
	}
	buf.WriteString(`]}`)
	return buf.Bytes()
}

// batchOverhead is what joinBatch adds around n items.
func batchOverhead(n int) int { return len(`{"items":[]}`) + max(n-1, 0) }

// Stream reads the broker's SSE stream until it ends, calling handle for each
// named event. It returns when the stream ends, ctx ends, handle fails, an
// event's data passes maxFrameBytes, or nothing arrives for streamIdle.
func (b *Broker) Stream(ctx context.Context, handle func(event string, data []byte) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := b.request(ctx, http.MethodGet, "/v1/bridge/stream", nil)
	if err != nil {
		return err
	}
	resp, err := b.stream.Do(req)
	if err != nil {
		return fmt.Errorf("broker stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("broker stream: status %d", resp.StatusCode)
	}
	idle := time.AfterFunc(streamIdle, cancel)
	defer idle.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	var event string
	var data bytes.Buffer
	for sc.Scan() {
		idle.Reset(streamIdle)
		line := sc.Text()
		switch {
		case line == "":
			if event != "" {
				if err := handle(event, bytes.Clone(data.Bytes())); err != nil {
					return fmt.Errorf("broker stream: %s: %w", event, err)
				}
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len()+len(line) > maxFrameBytes {
				return fmt.Errorf("broker stream: an event's data passes %d bytes", maxFrameBytes)
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("broker stream: %w", err)
	}
	return nil
}
