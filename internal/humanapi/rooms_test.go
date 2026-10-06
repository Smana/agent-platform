// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
)

// POST /api/rooms: an agents member creates a room they own and drive, in the
// broker's namespace.
func TestCreateRoom(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var createErr error
	rooms := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if createErr != nil {
				return createErr
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	srv := &Server{Humans: headerAuth{}, Groups: groups, Namespace: namespace, Actor: &Actor{Rooms: rooms}}
	post := func(user, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/rooms", strings.NewReader(body))
		if user != "" {
			req.Header.Set("X-Test-User", user)
		}
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		return rec
	}

	rec := post("alice", `{"dataClass":"internal","repository":"Smana/agent-platform"}`)
	var out struct{ ID string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !envelope.ValidID(out.ID) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var room v1alpha1.Room
	if err := rooms.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: out.ID}, &room); err != nil {
		t.Fatal(err)
	}
	if s := room.Spec; s.Owner != "human:alice" || s.Driver != "human:alice" || s.DataClass != "internal" || s.Repository != "Smana/agent-platform" ||
		len(s.Members) != 0 {
		t.Fatalf("%+v", s)
	}
	if rec := post("alice", `{"dataClass":"public"}`); rec.Code != http.StatusCreated {
		t.Fatalf("the repository is optional (the CRD defaults it): %d", rec.Code)
	}

	for _, c := range []struct {
		name, user, body string
		err              error
		want             int
	}{
		{"not authenticated", "", `{"dataClass":"public"}`, nil, http.StatusUnauthorized},
		{"not in an agents group", "stranger", `{"dataClass":"public"}`, nil, http.StatusForbidden},
		{"no data class", "alice", `{}`, nil, http.StatusBadRequest},
		{"another data class", "alice", `{"dataClass":"secret"}`, nil, http.StatusBadRequest},
		{"a malformed repository", "alice", `{"dataClass":"public","repository":"../etc"}`, nil, http.StatusBadRequest},
		{"an unknown field", "alice", `{"dataClass":"public","driver":"human:mallory"}`, nil, http.StatusBadRequest},
		{"not JSON", "alice", `dataClass=public`, nil, http.StatusBadRequest},
		// Review 4.4 M4: the fake client runs no CRD pattern, so the handler's own check is what refuses it.
		{"a principal the CRD would refuse", "a b", `{"dataClass":"public"}`, nil, http.StatusBadRequest},
		{"too large", "alice", `{"dataClass":"public","repository":"` + strings.Repeat("a", 5<<10) + `"}`, nil, http.StatusBadRequest},
		{"refused as invalid", "alice", `{"dataClass":"public"}`,
			apierrors.NewInvalid(schema.GroupKind{Group: "agents.ogenki.io", Kind: "Room"}, "x", field.ErrorList{}), http.StatusBadRequest},
		{"the API server is down", "alice", `{"dataClass":"public"}`, errors.New("down"), http.StatusServiceUnavailable},
	} {
		createErr = c.err
		if rec := post(c.user, c.body); rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
	}
	createErr = nil
	// Review 4.4 M4: room creation spends the human's action budget (burst 20).
	var codes []int
	for range 21 {
		codes = append(codes, post("bursty", `{"dataClass":"public"}`).Code)
	}
	if codes[19] != http.StatusCreated || codes[20] != http.StatusTooManyRequests {
		t.Fatalf("burst: %v", codes)
	}
	if rec := post("calm", `{"dataClass":"public"}`); rec.Code != http.StatusCreated {
		t.Fatalf("the budget is per human: %d", rec.Code)
	}
	srv.Actor = nil
	if rec := post("alice", `{"dataClass":"public"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no actor: %d", rec.Code)
	}
}

// GET /api/roomctl: what `roomctl configure` needs, for any agents member. The
// client id is read at use, like the web client's: the IdP mints it per build.
func TestRoomctlSetup(t *testing.T) {
	id := "3434@agents"
	srv := &Server{Humans: headerAuth{}, Groups: groups, PublicURL: "https://rooms.priv.example", Issuer: "https://auth.example",
		RoomctlClient: func() string { return id }}
	get := func(user string) (int, map[string]string) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/roomctl", nil)
		if user != "" {
			req.Header.Set("X-Test-User", user)
		}
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		var out map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, out := get("alice")
	if code != http.StatusOK || out["url"] != "https://rooms.priv.example" || out["issuer"] != "https://auth.example" || out["clientID"] != id {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := get("stranger"); code != http.StatusForbidden {
		t.Fatalf("not in an agents group: %d", code)
	}
	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Fatalf("not signed in: %d", code)
	}
	id = ""
	if code, out := get("alice"); code != http.StatusOK || out["clientID"] != "" {
		t.Fatalf("no roomctl client yet: %d %v", code, out)
	}
	srv.RoomctlClient = nil
	if code, out := get("alice"); code != http.StatusOK || out["clientID"] != "" {
		t.Fatalf("none configured: %d %v", code, out)
	}
}
