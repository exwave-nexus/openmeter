package main

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/openmeterio/openmeter/openmeter/server"
	"github.com/openmeterio/openmeter/pkg/clock"
)

type simulationClockRequest struct {
	Now              *time.Time `json:"now,omitempty"`
	To               *time.Time `json:"to,omitempty"`
	ExpectedRevision *uint64    `json:"expected_revision,omitempty"`
}

func registerSimulationClockRoutes(s *server.Server) {
	if os.Getenv("OPENMETER_SIMULATION_CLOCK_FILE") == "" {
		return
	}

	s.Get("/__sim/clock", func(w http.ResponseWriter, _ *http.Request) {
		snapshot, err := clock.GetSimulationSnapshot()
		writeSimulationClockResponse(w, snapshot, err)
	})
	s.Put("/__sim/clock", func(w http.ResponseWriter, r *http.Request) {
		var body simulationClockRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Now == nil {
			http.Error(w, "request requires now", http.StatusBadRequest)
			return
		}
		snapshot, err := clock.SetInitialSimulationTime(*body.Now)
		writeSimulationClockResponse(w, snapshot, err)
	})
	s.Post("/__sim/reset", func(w http.ResponseWriter, r *http.Request) {
		var body simulationClockRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Now == nil {
			http.Error(w, "request requires now", http.StatusBadRequest)
			return
		}
		snapshot, err := clock.ResetSimulationTime(*body.Now)
		writeSimulationClockResponse(w, snapshot, err)
	})
	s.Post("/__sim/clock/advance", func(w http.ResponseWriter, r *http.Request) {
		var body simulationClockRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.To == nil {
			http.Error(w, "request requires to", http.StatusBadRequest)
			return
		}
		snapshot, err := clock.AdvanceSimulationTime(*body.To, body.ExpectedRevision)
		writeSimulationClockResponse(w, snapshot, err)
	})
}

func writeSimulationClockResponse(w http.ResponseWriter, snapshot clock.SimulationSnapshot, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}
