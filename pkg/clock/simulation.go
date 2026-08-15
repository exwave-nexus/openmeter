package clock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const simulationClockFileEnv = "OPENMETER_SIMULATION_CLOCK_FILE"

type SimulationSnapshot struct {
	Now      time.Time `json:"now"`
	Revision uint64    `json:"revision"`
	Mode     string    `json:"mode"`
}

var simulationClockMu sync.Mutex

func simulationClockFile() string {
	return os.Getenv(simulationClockFileEnv)
}

func readSimulationSnapshot(path string) (SimulationSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SimulationSnapshot{}, err
	}

	var snapshot SimulationSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return SimulationSnapshot{}, fmt.Errorf("decode simulation clock: %w", err)
	}
	if snapshot.Now.IsZero() {
		return SimulationSnapshot{}, errors.New("simulation clock timestamp is missing")
	}

	snapshot.Now = snapshot.Now.UTC()
	snapshot.Mode = "paused"
	return snapshot, nil
}

func simulationNow() (time.Time, bool) {
	path := simulationClockFile()
	if path == "" {
		return time.Time{}, false
	}
	snapshot, err := readSimulationSnapshot(path)
	if err != nil {
		return time.Time{}, false
	}
	return snapshot.Now, true
}

func GetSimulationSnapshot() (SimulationSnapshot, error) {
	path := simulationClockFile()
	if path == "" {
		return SimulationSnapshot{}, errors.New("simulation clock is disabled")
	}
	snapshot, err := readSimulationSnapshot(path)
	if errors.Is(err, os.ErrNotExist) {
		return SimulationSnapshot{Now: time.Now().UTC(), Mode: "paused"}, nil
	}
	return snapshot, err
}

func SetInitialSimulationTime(now time.Time) (SimulationSnapshot, error) {
	return updateSimulationTime(now, nil, true)
}

func ResetSimulationTime(now time.Time) (SimulationSnapshot, error) {
	path := simulationClockFile()
	if path == "" {
		return SimulationSnapshot{}, errors.New("simulation clock is disabled")
	}
	now = now.UTC()
	simulationClockMu.Lock()
	defer simulationClockMu.Unlock()
	snapshot := SimulationSnapshot{Now: now, Revision: 0, Mode: "paused"}
	if err := writeSimulationSnapshot(path, snapshot); err != nil {
		return SimulationSnapshot{}, err
	}
	return snapshot, nil
}

func AdvanceSimulationTime(now time.Time, expectedRevision *uint64) (SimulationSnapshot, error) {
	return updateSimulationTime(now, expectedRevision, false)
}

func updateSimulationTime(now time.Time, expectedRevision *uint64, initial bool) (SimulationSnapshot, error) {
	now = now.UTC()
	path := simulationClockFile()
	if path == "" {
		return SimulationSnapshot{}, errors.New("simulation clock is disabled")
	}

	simulationClockMu.Lock()
	defer simulationClockMu.Unlock()

	current, err := readSimulationSnapshot(path)
	if errors.Is(err, os.ErrNotExist) {
		current = SimulationSnapshot{Now: now, Mode: "paused"}
	} else if err != nil {
		return SimulationSnapshot{}, err
	}
	if initial && current.Revision != 0 {
		return SimulationSnapshot{}, errors.New("initial time cannot be changed after the clock has advanced")
	}
	if expectedRevision != nil && *expectedRevision != current.Revision {
		return SimulationSnapshot{}, fmt.Errorf("clock revision mismatch: expected %d, current %d", *expectedRevision, current.Revision)
	}
	if now.Before(current.Now) && !initial {
		return SimulationSnapshot{}, errors.New("simulation clock cannot move backwards")
	}
	if now.After(current.Now) {
		current.Revision++
	}
	current.Now = now
	current.Mode = "paused"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return SimulationSnapshot{}, err
	}
	if err := writeSimulationSnapshot(path, current); err != nil {
		return SimulationSnapshot{}, err
	}

	return current, nil
}

func writeSimulationSnapshot(path string, snapshot SimulationSnapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
