package models

import (
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
)

// MockCadenceItem is a mock implementation of the Cadenced interface for testing.
type MockCadenceItem struct {
	ActiveFrom time.Time
	ActiveTo   *time.Time
}

func (m MockCadenceItem) GetCadence() CadencedModel {
	return CadencedModel(m)
}

func (m MockCadenceItem) cadence() CadencedModel {
	return CadencedModel(m)
}

// cadenced makes MockCadenceItem implement the Cadenced interface.
func (m MockCadenceItem) cadenced() cadencedMarker {
	return true // The actual value doesn't matter for these tests
}

var _ Cadenced = MockCadenceItem{} // Verify that MockCadenceItem implements Cadenced

func TestCadenceList_GetOverlaps(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{}
		expected := []OverlapDetail[MockCadenceItem]{}
		assert.ElementsMatch(t, expected, list.GetOverlaps(), "Elements should match in any order")
	})

	t.Run("no overlaps", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{
			{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC))},
			{ActiveFrom: time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 4, 0, 0, 0, 0, time.UTC))},
		}
		expected := []OverlapDetail[MockCadenceItem]{}
		assert.ElementsMatch(t, expected, list.GetOverlaps(), "Elements should match in any order")
	})

	t.Run("overlap with nil ActiveTo", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{
			{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
			{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC))},
		}
		expected := []OverlapDetail[MockCadenceItem]{
			{
				Index1: 0,
				Index2: 1,
				Item1:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
				Item2:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC))},
			},
		}
		assert.ElementsMatch(t, expected, list.GetOverlaps(), "Elements should match in any order")
	})

	t.Run("overlap with non-nil ActiveTo", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{
			{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC))},
			{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 4, 0, 0, 0, 0, time.UTC))},
		}
		expected := []OverlapDetail[MockCadenceItem]{
			{
				Index1: 0,
				Index2: 1,
				Item1:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC))},
				Item2:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 4, 0, 0, 0, 0, time.UTC))},
			},
		}
		assert.ElementsMatch(t, expected, list.GetOverlaps(), "Elements should match in any order")
	})

	t.Run("multiple overlaps", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{
			{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC))},
			{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 4, 0, 0, 0, 0, time.UTC))},
			{ActiveFrom: time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
			{ActiveFrom: time.Date(2023, 1, 5, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 6, 0, 0, 0, 0, time.UTC))},
		}
		expected := []OverlapDetail[MockCadenceItem]{
			{
				Index1: 0,
				Index2: 1,
				Item1:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC))},
				Item2:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 4, 0, 0, 0, 0, time.UTC))},
			},
			{
				Index1: 1,
				Index2: 2,
				Item1:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 4, 0, 0, 0, 0, time.UTC))},
				Item2:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
			},
			{
				Index1: 2,
				Index2: 3,
				Item1:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
				Item2:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 5, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 6, 0, 0, 0, 0, time.UTC))},
			},
		}
		assert.ElementsMatch(t, expected, list.GetOverlaps(), "Elements should match in any order")
	})

	t.Run("no overlap - adjacent", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{
			{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC))},
			{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC))},
		}
		expected := []OverlapDetail[MockCadenceItem]{}
		assert.ElementsMatch(t, expected, list.GetOverlaps(), "Elements should match in any order")
	})

	t.Run("single item", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{
			{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: lo.ToPtr(time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC))},
		}
		expected := []OverlapDetail[MockCadenceItem]{}
		assert.ElementsMatch(t, expected, list.GetOverlaps(), "Elements should match in any order")
	})

	t.Run("all nil ActiveTo", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{
			{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
			{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
			{ActiveFrom: time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
		}
		expected := []OverlapDetail[MockCadenceItem]{
			{
				Index1: 0,
				Index2: 1,
				Item1:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
				Item2:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
			},
			{
				Index1: 1,
				Index2: 2,
				Item1:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
				Item2:  MockCadenceItem{ActiveFrom: time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC), ActiveTo: nil},
			},
		}
		assert.ElementsMatch(t, expected, list.GetOverlaps(), "Elements should match in any order")
	})
}

func TestCadenceList_EmptyHistoryDoesNotCreateOrHideOverlap(t *testing.T) {
	start := time.Date(2025, time.July, 16, 0, 0, 13, 0, time.UTC)
	middle, end := start.Add(time.Hour), start.Add(2*time.Hour)
	for _, test := range []struct {
		name string
		list CadenceList[MockCadenceItem]
	}{
		{name: "empty before finite", list: CadenceList[MockCadenceItem]{{ActiveFrom: start, ActiveTo: &start}, {ActiveFrom: start, ActiveTo: &end}}},
		{name: "empty after finite at same start", list: CadenceList[MockCadenceItem]{{ActiveFrom: start, ActiveTo: &end}, {ActiveFrom: start, ActiveTo: &start}}},
		{name: "empty inside finite", list: CadenceList[MockCadenceItem]{{ActiveFrom: start, ActiveTo: &end}, {ActiveFrom: middle, ActiveTo: &middle}}},
		{name: "empty inside open ended", list: CadenceList[MockCadenceItem]{{ActiveFrom: start}, {ActiveFrom: middle, ActiveTo: &middle}}},
		{name: "empty between adjacent intervals", list: CadenceList[MockCadenceItem]{{ActiveFrom: start, ActiveTo: &middle}, {ActiveFrom: middle, ActiveTo: &middle}, {ActiveFrom: middle, ActiveTo: &end}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Empty(t, test.list.GetOverlaps())
		})
	}

	t.Run("empty history preserves a real overlap and its original indices", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{
			{ActiveFrom: start, ActiveTo: &end},
			{ActiveFrom: middle, ActiveTo: &middle},
			{ActiveFrom: middle},
		}
		assert.Equal(t, []OverlapDetail[MockCadenceItem]{{Index1: 0, Index2: 2, Item1: list[0], Item2: list[2]}}, list.GetOverlaps())
	})

	t.Run("inverted range is not treated as empty", func(t *testing.T) {
		list := CadenceList[MockCadenceItem]{
			{ActiveFrom: start, ActiveTo: &end},
			{ActiveFrom: middle, ActiveTo: &start},
		}
		assert.Equal(t, []OverlapDetail[MockCadenceItem]{{Index1: 0, Index2: 1, Item1: list[0], Item2: list[1]}}, list.GetOverlaps())
	})
}
