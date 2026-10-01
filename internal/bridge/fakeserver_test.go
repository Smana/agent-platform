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
	mu        sync.Mutex
	events    []map[string]any
	status    string
	sent      []string
	responses []bool
	policy    string
	pageSize  int
	limits    []int
	searches  int  // event searches asked
	hang      bool // event searches never answer while set
	flap      bool // each status read flips running and paused
	down      bool // every request is cut, as when agent-server is not listening
	cut       int  // event searches cut while down
	// failSearch, when set, runs under mu before an event search; true answers 500.
	failSearch func() bool
}

func (f *fakeAgentServer) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
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

func (f *fakeAgentServer) start(t *testing.T, conv string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	base := "/api/conversations/" + conv
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.flap {
			f.status = map[string]string{"running": "paused", "paused": "running"}[f.status]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": conv, "execution_status": f.status})
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
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Content) != 1 || in.Role != "user" || !in.Run {
			http.Error(w, "bad message", http.StatusUnprocessableEntity)
			return
		}
		f.mu.Lock()
		f.sent = append(f.sent, in.Content[0].Text)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})
	mux.HandleFunc("POST "+base+"/events/respond_to_confirmation", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Accept bool }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.responses = append(f.responses, in.Accept)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})
	mux.HandleFunc("POST "+base+"/confirmation_policy", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Policy struct{ Kind string } }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.policy = in.Policy.Kind
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})
	mux.HandleFunc("POST "+base+"/interrupt", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
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
