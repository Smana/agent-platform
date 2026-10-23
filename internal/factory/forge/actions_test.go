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
