// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"testing"
)

// actionsRig serves two runs of dead-links.yaml, run 7's findings artifact, and the storage
// GitHub redirects its download to: a path off the API, which must never see the token.
func actionsRig(t *testing.T) *rig {
	r := newRig(t)
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	w, _ := zw.Create("findings.json")
	_, _ = io.WriteString(w, `[{"file":"docs/a.md","line":3,"url":"https://example.com/gone","status":"404"}]`)
	_ = zw.Close()
	mux := r.gh.srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("GET /repos/Smana/demo/actions/workflows/dead-links.yaml/runs", r.gh.authed(func(w http.ResponseWriter, req *http.Request) {
		if q := req.URL.Query(); q.Get("branch") != "main" || q.Get("status") != "completed" {
			t.Errorf("runs query %v", q)
		}
		_, _ = io.WriteString(w, `{"total_count":2,"workflow_runs":[
		  {"id":6,"event":"schedule","head_branch":"main","conclusion":"success","created_at":"2026-10-21T04:23:00Z","triggering_actor":{"login":"Smana"}},
		  {"id":7,"event":"workflow_dispatch","head_branch":"main","conclusion":"success","created_at":"2026-10-22T04:23:00Z","triggering_actor":{"login":"Smana"}}]}`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/actions/runs/7/artifacts", r.gh.authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"total_count":2,"artifacts":[{"id":70,"name":"findings","expired":true},{"id":71,"name":"findings","expired":false}]}`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/actions/artifacts/71/zip", r.gh.authed(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", r.gh.srv.URL+"/storage/71.zip?sig=x")
		w.WriteHeader(http.StatusFound)
	}))
	mux.HandleFunc("GET /storage/71.zip", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			t.Error("the installation token reached the artifact storage")
		}
		_, _ = w.Write(zipped.Bytes())
	})
	return r
}

func TestWorkflowRunsNewestFirst(t *testing.T) {
	r := actionsRig(t)
	runs, err := r.g.WorkflowRuns(t.Context(), "dead-links.yaml")
	if err != nil || len(runs) != 2 || runs[0].ID != 7 || runs[0].Event != "workflow_dispatch" || runs[0].Actor != "Smana" ||
		runs[1].Branch != "main" || runs[1].Conclusion != "success" {
		t.Fatalf("%+v %v", runs, err)
	}
}

func TestRunArtifactReadsTheFileFromStorage(t *testing.T) {
	r := actionsRig(t)
	b, err := r.g.RunArtifact(t.Context(), 7, "findings", "findings.json")
	if err != nil || !bytes.Contains(b, []byte(`"status":"404"`)) {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := r.g.RunArtifact(t.Context(), 7, "findings", "other.json"); err == nil {
		t.Error("a file the zip does not hold is an error")
	}
	if _, err := r.g.RunArtifact(t.Context(), 7, "sarif", "x"); err == nil {
		t.Error("an artifact the run does not hold is an error")
	}
}

// storageRig is actionsRig with run 7's storage answering as serve says.
func storageRig(t *testing.T, serve http.HandlerFunc) *rig {
	t.Helper()
	r := actionsRig(t)
	mux := r.gh.srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("GET /repos/Smana/demo/actions/runs/8/artifacts", r.gh.authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"total_count":1,"artifacts":[{"id":81,"name":"findings","expired":false}]}`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/actions/artifacts/81/zip", r.gh.authed(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", r.gh.srv.URL+"/storage/81.zip?sig=x")
		w.WriteHeader(http.StatusFound)
	}))
	mux.HandleFunc("GET /storage/81.zip", serve)
	return r
}

// The download is bounded: a body past maxArtifactBytes is refused, not read whole.
func TestRunArtifactRefusesAnOversizedDownload(t *testing.T) {
	r := storageRig(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxArtifactBytes+1))
	})
	if b, err := r.g.RunArtifact(t.Context(), 8, "findings", "findings.json"); err == nil || len(b) != 0 {
		t.Fatalf("a download over %d bytes is refused: %d bytes, %v", maxArtifactBytes, len(b), err)
	}
}

// A small zip whose entry inflates past the cap (a zip bomb) is refused: stored zeros compress
// to a few KiB, so only the bound on the decompressed read stops it.
func TestRunArtifactRefusesAZipThatInflatesPastTheCap(t *testing.T) {
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "findings.json", Method: zip.Deflate})
	_, _ = w.Write(make([]byte, maxArtifactBytes+1))
	_ = zw.Close()
	if zipped.Len() > 64<<10 {
		t.Fatalf("the fixture must be small, is %d bytes", zipped.Len())
	}
	r := storageRig(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(zipped.Bytes()) })
	if b, err := r.g.RunArtifact(t.Context(), 8, "findings", "findings.json"); err == nil || len(b) != 0 {
		t.Fatalf("an entry inflating past %d bytes is refused: %d bytes, %v", maxArtifactBytes, len(b), err)
	}
}

// A storage answer other than 200 is an error, never a body to parse.
func TestRunArtifactRefusesANon200Storage(t *testing.T) {
	r := storageRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `[]`)
	})
	if b, err := r.g.RunArtifact(t.Context(), 8, "findings", "findings.json"); err == nil || len(b) != 0 {
		t.Fatalf("a 403 from storage is an error: %q %v", b, err)
	}
}
