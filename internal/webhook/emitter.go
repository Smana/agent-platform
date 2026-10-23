// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/summary"
)

const (
	pageSize     = 100
	queueSize    = 1024
	laggingLimit = 1000
	maxTrackers  = 10000
	// maxWorkers bounds the concurrent workers of one subscriber.
	maxWorkers = 8
	// maxRooms bounds the repository cache.
	maxRooms = 10000
	// disableAfter is how many given-up deliveries in a row disable a subscriber (R17).
	disableAfter = 3
)

// retryWaits are the waits before each retry of one delivery: one try, then three retries.
var retryWaits = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}

// Subscriber is one webhooks[] entry as the emitter needs it; internal/app maps the config's,
// since this package never imports config.
type Subscriber struct {
	Name, Network, URL, SecretFile string
	Events, Repositories           []string
	Generation                     int
}

// LogReader is the room log as the emitter reads it.
type LogReader interface {
	Range(ctx context.Context, roomID string, afterSeq int64, limit int) ([]envelope.Event, error)
	Listen(ctx context.Context, ready func(), fn func(roomID string, seq int64)) error
}

// StateStore is where each subscriber stands (R18) and whether it is disabled (R17).
type StateStore interface {
	Subscriber(ctx context.Context, sub string, generation int) (failures int, disabled bool, err error)
	WebhookCursor(ctx context.Context, sub, room string) (int64, error)
	SetWebhookCursor(ctx context.Context, sub, room string, seq int64) error
	Lagging(ctx context.Context, sub string, limit int) ([]string, error)
	Failed(ctx context.Context, sub string) (int, error)
	Succeeded(ctx context.Context, sub string) error
	Disable(ctx context.Context, sub string) error
}

// RoomRepo is a room's repository: "" when the Room is not known (yet) or names none (M5).
type RoomRepo func(ctx context.Context, room string) (string, error)

// Recorder counts deliveries and holds the disabled gauge.
type Recorder interface {
	WebhookDelivery(ctx context.Context, subscriber, outcome string)
	WebhookDisabled(ctx context.Context, subscriber string, disabled bool)
}

// Emitter posts each subscriber the events of its repositories' rooms, from the log (F6).
type Emitter struct {
	Subs    []Subscriber
	Events  LogReader
	Rooms   RoomRepo
	State   StateStore
	Metrics Recorder
	Logger  *slog.Logger
	Now     func() time.Time
	// Sleep waits between retries; nil waits on a timer or ctx. A test steps it.
	Sleep func(ctx context.Context, d time.Duration) error
	// SweepEvery is how often the dirty rooms are retried; 0 means 10 s.
	SweepEvery time.Duration
	// MinBackoff is the first wait before retrying a failed registration or reconnect; it
	// doubles to 30 s. 0 means 1 s.
	MinBackoff time.Duration
	// FactoryPrincipal is the system principal whose task facts name a room's repository.
	FactoryPrincipal string
	// LaggingLimit is how many rooms one catch-up asks for; 0 means 1,000.
	LaggingLimit int

	mu         sync.Mutex
	subs       []*subState
	running    map[workerKey]bool
	again      map[workerKey]bool // a notification came while the worker ran
	dirty      map[workerKey]bool // to retry on the next sweep
	mismatched map[workerKey]bool
	catchup    map[string]bool // subscribers whose Lagging must still run (or run again)
	rooms      map[string]*roomInfo
	trackers   map[workerKey]*list.Element
	lru        *list.List // of *trackerEntry, most recent first
	wg         sync.WaitGroup
}

type subState struct {
	Subscriber
	sem      chan struct{}
	client   *http.Client
	disabled atomic.Bool
}

// roomInfo is what the log (and, failing that, the Room) says of one room: its repository, how
// far the log was read for it, and whether the log is sealed. Shared by every subscriber.
type roomInfo struct {
	mu      sync.Mutex
	logRepo string // from the factory's task facts
	cr      string // the Room's spec.repository, once seen
	scanned int64
	sealed  bool
}

// resolved is what resolve knows of a room.
type resolved struct {
	repo     string
	sealed   bool
	scanned  int64
	mismatch bool // the log and the Room name different repositories
}

// brokerActor is who the broker seals a room's log as (store.sealTx).
const brokerActor = "system:room-broker"

// fromLog is the repository a task entry's issue or pull request URL names, and whether ev is
// the seal CloseRoom appends. A task entry counts only from the factory (a system actor of that
// principal, written through the system API), the seal only from the broker itself: any other
// writer of the room could otherwise name a repository or end the room. The event does not
// carry its origin client, so the factory's ":task" stream cannot be checked beyond that.
func (e *Emitter) fromLog(ev envelope.Event) (repo string, sealed bool) {
	if ev.Type != envelope.StateChanged || ev.Actor.Kind != envelope.ActorSystem {
		return "", false
	}
	var k struct {
		Kind  string `json:"kind"`
		Phase string `json:"phase"`
	}
	if json.Unmarshal(ev.Payload, &k) != nil {
		return "", false
	}
	switch {
	case k.Kind == "room_phase":
		return "", k.Phase == "Closed" && ev.Origin == envelope.OriginBroker && ev.Actor.ID == brokerActor
	case k.Kind == "task" && e.FactoryPrincipal != "" && ev.Origin == envelope.OriginClient && ev.Actor.ID == e.FactoryPrincipal:
		var f envelope.TaskFacts
		if json.Unmarshal(ev.Payload, &f) == nil {
			return summary.Repo(&f), false
		}
	}
	return "", false
}

type workerKey struct{ sub, room string }

type trackerEntry struct {
	key workerKey
	t   *Tracker
}

// NeedLeaderElection makes the emitter the leader's: two emitters would deliver everything twice.
func (e *Emitter) NeedLeaderElection() bool { return true }

// Start registers the subscribers, then delivers until ctx ends.
func (e *Emitter) Start(ctx context.Context) error {
	if e.Logger == nil {
		e.Logger = slog.New(slog.DiscardHandler)
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.SweepEvery <= 0 {
		e.SweepEvery = 10 * time.Second
	}
	if e.MinBackoff <= 0 {
		e.MinBackoff = time.Second
	}
	if e.LaggingLimit <= 0 {
		e.LaggingLimit = laggingLimit
	}
	e.running, e.again, e.dirty = map[workerKey]bool{}, map[workerKey]bool{}, map[workerKey]bool{}
	e.catchup, e.rooms = map[string]bool{}, map[string]*roomInfo{}
	e.mismatched = map[workerKey]bool{}
	e.trackers, e.lru = map[workerKey]*list.Element{}, list.New()
	for _, s := range e.Subs {
		st := &subState{Subscriber: s, sem: make(chan struct{}, maxWorkers)}
		disabled, err := e.register(ctx, s)
		if err != nil {
			return nil // ctx ended
		}
		st.disabled.Store(disabled)
		e.Metrics.WebhookDisabled(ctx, s.Name, disabled)
		if s.Network == "internet" {
			st.client = SafeClient(10 * time.Second)
		} else {
			st.client = httpx.New(10*time.Second, nil)
			st.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		}
		e.subs = append(e.subs, st)
	}
	queue := make(chan string, queueSize)
	e.wg.Add(2)
	go func() { defer e.wg.Done(); e.dispatch(ctx, queue) }()
	go func() { defer e.wg.Done(); e.sweep(ctx) }()
	e.listen(ctx, queue)
	e.wg.Wait()
	return nil
}

// register records the subscriber in the store, retrying with a backoff until it works or ctx
// ends: a database blip at startup must not stop the manager.
func (e *Emitter) register(ctx context.Context, s Subscriber) (disabled bool, err error) {
	backoff := e.MinBackoff
	for {
		_, disabled, err = e.State.Subscriber(ctx, s.Name, s.Generation)
		if err == nil {
			return disabled, nil
		}
		e.Logger.Warn("webhook: register subscriber", "subscriber", s.Name, "error", err, "retryIn", backoff)
		if err := wait(ctx, backoff); err != nil {
			return false, err
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// listen holds the LISTEN session, reconnecting with a backoff, and on each ready (the first
// connect and every reconnect) marks the rooms past each cursor dirty: notifications sent while
// the listener was down are gone.
func (e *Emitter) listen(ctx context.Context, queue chan<- string) {
	backoff := e.MinBackoff
	for ctx.Err() == nil {
		err := e.Events.Listen(ctx, func() {
			backoff = e.MinBackoff
			e.mu.Lock()
			for _, s := range e.subs {
				e.catchup[s.Name] = true
			}
			e.mu.Unlock()
			e.catchUp(ctx)
		}, func(room string, _ int64) {
			select {
			case queue <- room:
			default: // fn must not block: the sweep picks the room up
				e.markDirty(room)
			}
		})
		if ctx.Err() != nil {
			return
		}
		e.Logger.Warn("webhook listener lost; reconnecting", "error", err, "retryIn", backoff)
		if wait(ctx, backoff) != nil {
			return
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// catchUp marks dirty the rooms past each pending subscriber's cursor. A subscriber stays
// pending until Lagging succeeded and returned fewer rooms than the limit, so a failed call or a
// truncated list is retried by the sweep.
func (e *Emitter) catchUp(ctx context.Context) {
	for _, s := range e.subs {
		e.mu.Lock()
		pending := e.catchup[s.Name]
		e.mu.Unlock()
		if !pending {
			continue
		}
		rooms, err := e.State.Lagging(ctx, s.Name, e.LaggingLimit)
		if err != nil {
			e.Logger.Warn("webhook: lagging rooms", "error", err)
			continue
		}
		e.mu.Lock()
		for _, room := range rooms {
			e.dirty[workerKey{s.Name, room}] = true
		}
		e.catchup[s.Name] = len(rooms) >= e.LaggingLimit
		e.mu.Unlock()
	}
}

func (e *Emitter) markDirty(room string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.subs {
		e.dirty[workerKey{s.Name, room}] = true
	}
}

// dispatch hands each notified room to every subscriber's worker.
func (e *Emitter) dispatch(ctx context.Context, queue <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case room := <-queue:
			for _, s := range e.subs {
				e.kick(ctx, s, room)
			}
		}
	}
}

// sweep retries the dirty rooms: held for an unknown repository, overflowed, or lagging.
func (e *Emitter) sweep(ctx context.Context) {
	tick := time.NewTicker(e.SweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		e.catchUp(ctx)
		e.mu.Lock()
		keys := make([]workerKey, 0, len(e.dirty))
		for k := range e.dirty {
			keys = append(keys, k)
		}
		clear(e.dirty)
		e.mu.Unlock()
		for _, k := range keys {
			for _, s := range e.subs {
				if s.Name == k.sub {
					e.kick(ctx, s, k.room)
				}
			}
		}
	}
}

// kick runs a worker for (s, room), or asks the one that runs to look again (M7: one worker per
// subscriber and room, so a dead subscriber's retry waits hold no other subscriber back).
func (e *Emitter) kick(ctx context.Context, s *subState, room string) {
	key := workerKey{s.Name, room}
	e.mu.Lock()
	if e.running[key] {
		e.again[key] = true
		e.mu.Unlock()
		return
	}
	e.running[key] = true
	e.mu.Unlock()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			select {
			case s.sem <- struct{}{}:
			case <-ctx.Done():
				e.mu.Lock()
				delete(e.running, key)
				delete(e.again, key)
				e.mu.Unlock()
				return
			}
			held := e.work(ctx, s, room)
			<-s.sem
			e.mu.Lock()
			if held {
				e.dirty[key] = true
			}
			if !e.again[key] || ctx.Err() != nil {
				delete(e.running, key)
				delete(e.again, key)
				e.mu.Unlock()
				return
			}
			delete(e.again, key)
			e.mu.Unlock()
		}
	}()
}

// work delivers room's pending events to s. held is true when it must be retried later without
// a new notification: the repository is unknown (M5) or a store call failed.
func (e *Emitter) work(ctx context.Context, s *subState, room string) (held bool) {
	res, err := e.resolve(ctx, room)
	repo, sealed, scanned := res.repo, res.sealed, res.scanned
	if err == nil && res.mismatch {
		key := workerKey{s.Name, room}
		e.mu.Lock()
		first := !e.mismatched[key]
		e.mismatched[key] = true
		e.mu.Unlock()
		if first { // once per subscriber and room, not once per sweep
			e.Logger.Warn("webhook: the room's log and its Room name different repositories; held", "subscriber", s.Name, "room", room)
			e.Metrics.WebhookDelivery(ctx, s.Name, "mismatch")
		}
		return true
	}
	if err != nil {
		e.Logger.Warn("webhook: resolve repository", "subscriber", s.Name, "room", room, "error", err)
		return true
	}
	cur, err := e.State.WebhookCursor(ctx, s.Name, room)
	if err != nil {
		e.Logger.Warn("webhook: read cursor", "subscriber", s.Name, "room", room, "error", err)
		return true
	}
	if repo == "" {
		if !sealed {
			return true // the Room may appear, and its events are then still wanted (M5)
		}
		// A closed room whose repository nothing names: its events cannot be matched to a
		// subscriber. Moving on keeps it from staying dirty forever.
		if scanned > cur {
			if err := e.State.SetWebhookCursor(ctx, s.Name, room, scanned); err != nil {
				return true
			}
			e.Metrics.WebhookDelivery(ctx, s.Name, "unresolved")
		}
		return false
	}
	var tr *Tracker
	if slices.ContainsFunc(s.Repositories, func(r string) bool { return strings.EqualFold(r, repo) }) {
		if tr, err = e.tracker(ctx, workerKey{s.Name, room}, repo, cur); err != nil {
			e.Logger.Warn("webhook: warm tracker", "subscriber", s.Name, "room", room, "error", err)
			return true
		}
	}
	for {
		evs, err := e.Events.Range(ctx, room, cur, pageSize)
		if err != nil {
			e.Logger.Warn("webhook: read room", "subscriber", s.Name, "room", room, "error", err)
			return true
		}
		for _, ev := range evs {
			if tr != nil {
				// A disabled subscriber's Tracker still reads, so a re-enabled one is current (M12).
				for _, out := range tr.Events(room, repo, ev) {
					if s.disabled.Load() || !slices.Contains(s.Events, out.Type) {
						continue
					}
					if !e.deliver(ctx, s, room, ev.Seq, out) {
						e.dropTracker(workerKey{s.Name, room})
						return false // ctx ended; the cursor stays
					}
				}
			}
			// A disabled or foreign-repository subscriber's cursor advances too (R17, M12).
			if err := e.State.SetWebhookCursor(ctx, s.Name, room, ev.Seq); err != nil {
				e.Logger.Warn("webhook: move cursor", "subscriber", s.Name, "room", room, "error", err)
				e.dropTracker(workerKey{s.Name, room}) // it read ev; re-warm rather than ignore the replay
				return true
			}
			cur = ev.Seq
		}
		if len(evs) < pageSize {
			return false
		}
	}
}

// resolve is a room's repository: from its own log first (the URL of its issue or pull request in
// the factory's task facts), so a closed room whose Room is gone still resolves, and the Room's
// spec.repository when the log has none. When both exist and differ (case aside) the room is a
// mismatch and nothing is delivered. It reads the log once, then only what was appended since.
func (e *Emitter) resolve(ctx context.Context, room string) (resolved, error) {
	e.mu.Lock()
	info := e.rooms[room]
	if info == nil {
		if len(e.rooms) >= maxRooms {
			for k := range e.rooms {
				delete(e.rooms, k)
				break
			}
		}
		info = &roomInfo{}
		e.rooms[room] = info
	}
	e.mu.Unlock()
	info.mu.Lock()
	defer info.mu.Unlock()
	for info.logRepo == "" {
		evs, err := e.Events.Range(ctx, room, info.scanned, pageSize)
		if err != nil {
			return resolved{}, err
		}
		for _, ev := range evs {
			info.scanned = ev.Seq
			r, seal := e.fromLog(ev)
			info.sealed = info.sealed || seal
			if r != "" {
				info.logRepo = r
				break
			}
		}
		if len(evs) < pageSize {
			break
		}
	}
	if info.cr == "" {
		r, err := e.Rooms(ctx, room)
		if err != nil {
			return resolved{}, err
		}
		info.cr = r // "" when the Room is gone or not in the cache yet: asked again next time
	}
	res := resolved{sealed: info.sealed, scanned: info.scanned}
	switch {
	case info.logRepo != "" && info.cr != "" && !strings.EqualFold(info.logRepo, info.cr):
		res.mismatch = true
	case info.logRepo != "":
		res.repo = info.logRepo
	default:
		res.repo = info.cr
	}
	return res, nil
}

// tracker is (s, room)'s Tracker (M1), warmed by replaying the room up to cur when missing.
func (e *Emitter) tracker(ctx context.Context, key workerKey, repo string, cur int64) (*Tracker, error) {
	e.mu.Lock()
	if el, ok := e.trackers[key]; ok {
		e.lru.MoveToFront(el)
		t := el.Value.(*trackerEntry).t
		e.mu.Unlock()
		return t, nil
	}
	e.mu.Unlock()
	t := &Tracker{}
	for after := int64(0); after < cur; {
		evs, err := e.Events.Range(ctx, key.room, after, pageSize)
		if err != nil {
			return nil, err
		}
		for _, ev := range evs {
			if ev.Seq > cur {
				break
			}
			t.Events(key.room, repo, ev) // output discarded: the subscriber is past these
			after = ev.Seq
		}
		if len(evs) < pageSize {
			break
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.trackers[key] = e.lru.PushFront(&trackerEntry{key: key, t: t})
	if e.lru.Len() > maxTrackers {
		old := e.lru.Back()
		e.lru.Remove(old)
		delete(e.trackers, old.Value.(*trackerEntry).key)
	}
	return t, nil
}

func (e *Emitter) dropTracker(key workerKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if el, ok := e.trackers[key]; ok {
		e.lru.Remove(el)
		delete(e.trackers, key)
	}
}

// deliver posts ev with its retries. It returns false only when ctx ended; a delivery given up
// is counted against the subscriber and returns true, so the cursor moves on.
func (e *Emitter) deliver(ctx context.Context, s *subState, room string, seq int64, ev Event) bool {
	body, err := json.Marshal(ev)
	if err != nil {
		return true
	}
	id := ID(s.Name, room, seq, ev.Type)
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		if s.disabled.Load() {
			return true // disabled while retrying: stop, the cursor moves on
		}
		outcome := e.post(ctx, s, id, body)
		if outcome == "delivered" {
			e.Metrics.WebhookDelivery(ctx, s.Name, outcome)
			if err := e.State.Succeeded(ctx, s.Name); err != nil {
				e.Logger.Warn("webhook: record success", "subscriber", s.Name, "error", err)
			}
			return true
		}
		if attempt == len(retryWaits) {
			if outcome != "no_key" { // an unreadable key file is the cause, whichever attempt it was
				outcome = "failed"
			}
			e.Metrics.WebhookDelivery(ctx, s.Name, outcome)
			break
		}
		if outcome != "no_key" {
			outcome = "retried"
		}
		e.Metrics.WebhookDelivery(ctx, s.Name, outcome)
		if e.sleep(ctx, retryWaits[attempt]) != nil {
			return false
		}
	}
	n, err := e.State.Failed(ctx, s.Name)
	if err != nil {
		e.Logger.Warn("webhook: record failure", "subscriber", s.Name, "error", err)
		return true
	}
	if n >= disableAfter {
		if err := e.State.Disable(ctx, s.Name); err != nil {
			e.Logger.Warn("webhook: disable", "subscriber", s.Name, "error", err)
			return true
		}
		s.disabled.Store(true)
		e.Metrics.WebhookDisabled(ctx, s.Name, true)
		e.Logger.Warn("webhook subscriber disabled", "subscriber", s.Name)
	}
	return true
}

// post is one attempt: "delivered" on a 2xx, "no_key" when the key file cannot be read (it is
// re-read each time, so a rotation needs no restart), else "error".
func (e *Emitter) post(ctx context.Context, s *subState, id string, body []byte) string {
	keys, err := ReadKeys(s.SecretFile)
	if err != nil {
		e.Logger.Warn("webhook: signing keys unreadable", "subscriber", s.Name)
		return "no_key"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return "error"
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range Sign(keys, id, e.Now(), body) {
		req.Header[k] = v
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "error"
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		return "delivered"
	}
	return "error"
}

func (e *Emitter) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	return wait(ctx, d)
}

// wait is a timer or ctx.
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
