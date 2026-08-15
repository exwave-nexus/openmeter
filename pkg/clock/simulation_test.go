package clock

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSimulationClockLifecycle(t *testing.T) {
	t.Setenv(simulationClockFileEnv, filepath.Join(t.TempDir(), "clock.json"))
	initial := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	snapshot, err := SetInitialSimulationTime(initial)
	require.NoError(t, err)
	require.Equal(t, initial, snapshot.Now)
	require.Zero(t, snapshot.Revision)
	require.Equal(t, initial, Now())

	revision := uint64(0)
	next := initial.Add(7 * 24 * time.Hour)
	snapshot, err = AdvanceSimulationTime(next, &revision)
	require.NoError(t, err)
	require.Equal(t, uint64(1), snapshot.Revision)
	require.Equal(t, next, Now())

	_, err = AdvanceSimulationTime(next.Add(time.Hour), &revision)
	require.ErrorContains(t, err, "revision mismatch")
	_, err = AdvanceSimulationTime(initial, nil)
	require.ErrorContains(t, err, "cannot move backwards")
}

func TestSimulationClockDisabled(t *testing.T) {
	t.Setenv(simulationClockFileEnv, "")
	_, err := GetSimulationSnapshot()
	require.ErrorContains(t, err, "disabled")
}
