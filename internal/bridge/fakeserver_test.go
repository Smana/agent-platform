// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeAgentServer is agent-server 1.49.6's loopback contract as read from its
// event_router.py and event_service.py: page_id is inclusive, and next_page_id is
// the id of the first event of the next page.
type fakeAgentServer struct {
	mu         sync.Mutex
	events     []map[string]any
	status     string
	sent       []string
	responses  []bool
	reasons    []string // the reason of each response
	refuse     int      // while set, confirmations and the policy are answered with this status
	policy     string
	policySets int // confirmation policies taken
	pageSize   int
	limits     []int
	searches   int  // event searches asked
	hang       bool // event searches never answer while set
	flap       bool // each status read flips running and paused
	// writes logs each write in order: "respond true|false", "send", "run", "interrupt".
	writes  []string
	runCode int // while set, POST /run is answered with this status
	// runTakenCode: POST /run runs the conversation, then answers this status.
	runTakenCode int
	statusCode   int // while set, the conversation read is answered with this status
	// parkAfterRead makes the running step park on a confirmation right after
	// the next status read, unless a message arrived during it.
	parkAfterRead bool
	messaged      bool // a message arrived during the current step
	down          bool // every request is cut, as when agent-server is not listening
	cut           int  // event searches cut while down
	// failSearch, when set, runs under mu before an event search; true answers 500.
	failSearch func() bool
}

func (f *fakeAgentServer) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// runStep is run() (local_conversation.py:2191-2197, agent.py:652-661): on a
// conversation waiting for a confirmation, or paused with actions unmatched,
// it runs every pending action, an implicit confirmation. Under f.mu.
func (f *fakeAgentServer) runStep() {
	if f.status == "waiting_for_confirmation" || f.status == "paused" {
		f.writes = append(f.writes, "implicit accept")
	}
	if f.status != "running" {
		f.messaged = false // a new step
	}
	f.status = "running"
}

// written copies the writes received so far, in order.
func (f *fakeAgentServer) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.writes...)
}

// searched is how many event searches were asked so far.
func (f *fakeAgentServer) searched() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searches
}

func (f *fakeAgentServer) add(ev map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev["id"] = "e" + strconv.Itoa(len(f.events)+1)
	f.events = append(f.events, ev)
}

// addAsIs appends ev with whatever id it has, or none: a malformed event.
func (f *fakeAgentServer) addAsIs(ev map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

// asked is the limit of every page request so far.
func (f *fakeAgentServer) asked() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int{}, f.limits...)
}

// snapshot copies what the fake received, under its lock (the gate runs -race).
func (f *fakeAgentServer) snapshot() (sent []string, responses []bool, policy string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sent...), append([]bool{}, f.responses...), f.policy
}

// policySet is the confirmation policy taken last, and how many were taken.
func (f *fakeAgentServer) policySet() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy, f.policySets
}

// answers copies the reasons of the confirmations answered so far.
func (f *fakeAgentServer) answers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.reasons...)
}

func (f *fakeAgentServer) start(t *testing.T, conv string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	base := "/api/conversations/" + conv
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if code := f.statusCode; code != 0 {
			http.Error(w, "refused", code)
			return
		}
		if f.flap {
			f.status = map[string]string{"running": "paused", "paused": "running"}[f.status]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": conv, "execution_status": f.status})
		if f.parkAfterRead && f.status == "running" {
			// The running step ends right after this read and asks to park.
			f.parkAfterRead = false
			if f.messaged {
				// local_conversation.py:2289-2324: the live loop rejects the
				// park and goes on with the message.
				f.writes = append(f.writes, "rejected by the message")
			} else {
				f.status = "waiting_for_confirmation"
			}
		}
	})
	mux.HandleFunc("GET "+base+"/events/search", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.searches++
		if f.hang {
			f.mu.Unlock()
			<-r.Context().Done() // a hung agent-server: the caller's deadline ends it
			return
		}
		defer f.mu.Unlock()
		if f.failSearch != nil && f.failSearch() {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		start := 0
		if id := r.URL.Query().Get("page_id"); id != "" {
			for i, e := range f.events {
				if e["id"] == id {
					start = i
				}
			}
		}
		size := f.pageSize
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
			f.limits = append(f.limits, l)
			size = min(size, l)
		}
		end := min(start+size, len(f.events))
		var next any
		if end < len(f.events) {
			next = f.events[end]["id"]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": f.events[start:end], "next_page_id": next})
	})
	mux.HandleFunc("POST "+base+"/events", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Role    string                  `json:"role"`
			Content []struct{ Text string } `json:"content"`
			Run     bool                    `json:"run"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Content) != 1 || in.Role != "user" {
			http.Error(w, "bad message", http.StatusUnprocessableEntity)
			return
		}
		f.mu.Lock()
		f.sent = append(f.sent, in.Content[0].Text)
		f.messaged = true
		if in.Run {
			f.writes = append(f.writes, "send run")
			f.runStep()
		} else {
			f.writes = append(f.writes, "send")
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})
	mux.HandleFunc("POST "+base+"/events/respond_to_confirmation", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Accept bool
			Reason string
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		if code := f.refuse; code != 0 {
			f.mu.Unlock()
			http.Error(w, "refused", code)
			return
		}
		f.responses = append(f.responses, in.Accept)
		f.reasons = append(f.reasons, in.Reason)
		f.writes = append(f.writes, "respond "+strconv.FormatBool(in.Accept))
		f.status = "running"
		if !in.Accept {
			f.status = "idle" // OpenHands rejects, goes idle, and does not run
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})
	mux.HandleFunc("POST "+base+"/confirmation_policy", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Policy struct{ Kind string } }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		if code := f.refuse; code != 0 {
			f.mu.Unlock()
			http.Error(w, "refused", code)
			return
		}
		f.policy = in.Policy.Kind
		f.policySets++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})
	mux.HandleFunc("POST "+base+"/run", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.writes = append(f.writes, "run")
		switch {
		case f.runCode != 0:
			http.Error(w, "refused", f.runCode)
			return
		case f.runTakenCode != 0:
			f.runStep() // taken, but the answer is lost on the way back
			http.Error(w, "gateway timeout", f.runTakenCode)
			return
		case f.status == "running":
			http.Error(w, "already running", http.StatusConflict)
			return
		}
		f.runStep()
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})
	mux.HandleFunc("POST "+base+"/interrupt", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.writes = append(f.writes, "interrupt")
		f.status = "paused"
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.down
		if down && strings.HasSuffix(r.URL.Path, "/events/search") {
			f.cut++
		}
		f.mu.Unlock()
		if down {
			if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = c.Close()
			}
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}
