package subscription

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openmeterio/openmeter/pkg/clock"
	"github.com/openmeterio/openmeter/pkg/datetime"
)

func TestCurrentPeriodEditTiming(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 13, 0, time.UTC)
	periodStart := start.AddDate(0, 0, 7)
	clock.FreezeTime(periodStart.AddDate(0, 0, 2))
	defer clock.UnFreeze()

	view := SubscriptionView{Spec: SubscriptionSpec{
		CreateSubscriptionPlanInput:     CreateSubscriptionPlanInput{BillingCadence: datetime.MustParseDuration(t, "P1W")},
		CreateSubscriptionCustomerInput: CreateSubscriptionCustomerInput{ActiveFrom: start, BillingAnchor: start},
		Phases: map[string]*SubscriptionPhaseSpec{
			"main": {CreateSubscriptionPhasePlanInput: CreateSubscriptionPhasePlanInput{
				PhaseKey: "main", StartAfter: datetime.MustParseDuration(t, "P0D"),
			}},
		},
	}}
	for _, tc := range []struct {
		name    string
		at      time.Time
		allowed bool
	}{
		{name: "current period start", at: periodStart, allowed: true},
		{name: "minute-truncated entitlement period", at: periodStart.Truncate(time.Minute)},
		{name: "prior closed period", at: start},
		{name: "arbitrary time in current period", at: periodStart.Add(time.Hour)},
		{name: "arbitrary custom future", at: periodStart.AddDate(0, 0, 8)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timing := Timing{Custom: &tc.at}
			if tc.allowed {
				require.NoError(t, timing.ValidateForCurrentPeriodEdit(&view))
			} else {
				require.Error(t, timing.ValidateForCurrentPeriodEdit(&view))
			}
			// Only the running-edit workflow opts into this exception.
			require.Error(t, timing.ValidateForAction(SubscriptionActionUpdate, &view))
		})
	}

	t.Run("current-period plan replacement", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			at      time.Time
			allowed bool
		}{
			{name: "delayed reset in current period", at: periodStart.Add(time.Hour), allowed: true},
			{name: "current boundary", at: periodStart, allowed: true},
			{name: "closed period", at: periodStart.Add(-time.Second)},
			{name: "future", at: clock.Now().Add(time.Second)},
			{name: "fractional second", at: periodStart.Add(time.Hour + time.Millisecond)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				timing := Timing{Custom: &tc.at}
				if tc.allowed {
					require.NoError(t, timing.ValidateForCurrentPeriodPlanChange(&view, &tc.at))
				} else {
					require.Error(t, timing.ValidateForCurrentPeriodPlanChange(&view, &tc.at))
				}
			})
		}
		at := periodStart.Add(time.Hour)
		mismatched := at.Add(time.Second)
		require.Error(t, (Timing{Custom: &at}).ValidateForCurrentPeriodPlanChange(&view, &mismatched))
		require.Error(t, (Timing{Custom: &at}).ValidateForAction(SubscriptionActionCancel, &view))
		require.Error(t, (Timing{Custom: &at}).ValidateForAction(SubscriptionActionCreate, nil))
	})

	t.Run("phase boundary does not permit editing previous phase", func(t *testing.T) {
		phaseStart := periodStart.AddDate(0, 0, 1)
		view.Spec.Phases["next"] = &SubscriptionPhaseSpec{CreateSubscriptionPhasePlanInput: CreateSubscriptionPhasePlanInput{
			PhaseKey: "next", StartAfter: datetime.ISODurationBetween(start, phaseStart),
		}}
		require.Error(t, (Timing{Custom: &periodStart}).ValidateForCurrentPeriodEdit(&view))
		require.NoError(t, (Timing{Custom: &phaseStart}).ValidateForCurrentPeriodEdit(&view))
		require.Error(t, (Timing{Custom: &periodStart}).ValidateForCurrentPeriodPlanChange(&view, &periodStart))
	})
}
