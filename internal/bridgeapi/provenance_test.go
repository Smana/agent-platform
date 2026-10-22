// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// R10: the broker stores a run's provenance with the task's facts. Before this change its strict
// decoder answered 400 bad_message.
func TestTaskFactsCarryARunsProvenance(t *testing.T) {
	s, log, _ := newServer(t)
	body := []byte(`{"clientSeq":7,"facts":{"phase":"Implementing","run":{"id":"cf4ato2x","role":"implementer",` +
		`"provenance":{"factoryVersion":"v0.16.0","servedModels":["glm-5.3"]}}}}`)
	rec := call(t, s.Routes(), http.MethodPost, "/v1/rooms/"+room+"/task", "sys:"+factory, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	evs, err := log.Range(t.Context(), room, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Run struct {
			Provenance struct {
				FactoryVersion string   `json:"factoryVersion"`
				ServedModels   []string `json:"servedModels"`
			} `json:"provenance"`
		} `json:"run"`
	}
	if len(evs) != 1 {
		t.Fatalf("events %+v", evs)
	}
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil || p.Run.Provenance.FactoryVersion != "v0.16.0" ||
		len(p.Run.Provenance.ServedModels) != 1 {
		t.Fatalf("payload %s: %v", evs[0].Payload, err)
	}
}
