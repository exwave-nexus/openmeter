package service_test

import (
	"testing"
	"time"

	"github.com/alpacahq/alpacadecimal"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/openmeterio/openmeter/openmeter/billing/worker/subscriptionsync/service/targetstate"
	"github.com/openmeterio/openmeter/openmeter/productcatalog"
	"github.com/openmeterio/openmeter/openmeter/subscription"
	"github.com/openmeterio/openmeter/openmeter/subscription/patch"
	subscriptiontestutils "github.com/openmeterio/openmeter/openmeter/subscription/testutils"
	subscriptionworkflow "github.com/openmeterio/openmeter/openmeter/subscription/workflow"
	"github.com/openmeterio/openmeter/openmeter/testutils"
	"github.com/openmeterio/openmeter/pkg/clock"
	"github.com/openmeterio/openmeter/pkg/datetime"
	"github.com/openmeterio/openmeter/pkg/models"
)

func TestCurrentPeriodQuotaUpgradePreservesUsageAndHistoricalPeriods(t *testing.T) {
	// given: a weekly Hobby allowance and an already consumed earlier week.
	start := time.Date(2026, 1, 5, 12, 0, 13, 0, time.UTC)
	clock.FreezeTime(start)
	defer clock.UnFreeze()
	ctx := t.Context()
	dbDeps := subscriptiontestutils.SetupDBDeps(t)
	defer dbDeps.Cleanup(t)
	deps := subscriptiontestutils.NewService(t, dbDeps)
	deps.FeatureConnector.CreateExampleFeatures(t, deps.ExampleMeterID)
	customer := deps.CustomerAdapter.CreateExampleCustomer(t)
	week := datetime.MustParseDuration(t, "P1W")
	card := subscriptiontestutils.ExampleRateCard1
	card.BillingCadence = week
	card.EntitlementTemplate = productcatalog.NewEntitlementTemplateFrom(productcatalog.MeteredEntitlementTemplate{
		IssueAfterReset: lo.ToPtr(200.0), UsagePeriod: week, IsSoftLimit: true,
	})
	card.Discounts = productcatalog.Discounts{Usage: &productcatalog.UsageDiscount{Quantity: alpacadecimal.NewFromInt(200)}}
	card.Price = productcatalog.NewPriceFrom(productcatalog.UnitPrice{Amount: alpacadecimal.NewFromFloat(0.01)})
	input := subscriptiontestutils.BuildTestPlanInput(t).AddPhase(nil, card.Clone()).Build()
	input.Key = "hobby"
	input.BillingCadence = week
	plan := deps.PlanHelper.CreatePlan(t, input)
	view, err := deps.WorkflowService.CreateFromPlan(ctx, subscriptionworkflow.CreateSubscriptionWorkflowInput{
		Namespace: subscriptiontestutils.ExampleNamespace, CustomerID: customer.ID,
		ChangeSubscriptionWorkflowInput: subscriptionworkflow.ChangeSubscriptionWorkflowInput{Timing: subscription.Timing{Custom: &start}},
	}, plan)
	require.NoError(t, err)
	phaseKey := view.Phases[0].SubscriptionPhase.Key
	featureKey := subscriptiontestutils.ExampleFeatureKey
	periodStart := start.AddDate(0, 0, 7)
	deps.MockStreamingConnector.AddSimpleEvent(subscriptiontestutils.ExampleFeatureMeterSlug, 230, start)
	deps.MockStreamingConnector.AddSimpleEvent(subscriptiontestutils.ExampleFeatureMeterSlug, 200, periodStart)
	clock.FreezeTime(periodStart.AddDate(0, 0, 2))
	defer clock.UnFreeze()

	historical, err := deps.EntitlementRegistry.MeteredEntitlement.GetEntitlementBalance(ctx, models.NamespacedID{
		Namespace: view.Subscription.Namespace, ID: view.Phases[0].ItemsByKey[featureKey][0].Entitlement.Entitlement.ID,
	}, periodStart.Add(-2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 230.0, historical.UsageInPeriod)
	require.Equal(t, 30.0, historical.Overage)

	// when: Indie replaces the rate card at this week's start, preserving phase identity.
	card.EntitlementTemplate = productcatalog.NewEntitlementTemplateFrom(productcatalog.MeteredEntitlementTemplate{
		IssueAfterReset: lo.ToPtr(1_000.0), UsagePeriod: week, IsSoftLimit: true,
	})
	card.Discounts.Usage = &productcatalog.UsageDiscount{Quantity: alpacadecimal.NewFromInt(1_000)}
	card.Metadata = models.Metadata{"exwave_plan_key": "indie"}
	view, err = deps.WorkflowService.EditRunning(ctx, view.Subscription.NamespacedID, []subscription.Patch{
		patch.PatchAddItem{PhaseKey: phaseKey, ItemKey: featureKey, CreateInput: subscription.SubscriptionItemSpec{
			CreateSubscriptionItemInput: subscription.CreateSubscriptionItemInput{CreateSubscriptionItemPlanInput: subscription.CreateSubscriptionItemPlanInput{
				PhaseKey: phaseKey, ItemKey: featureKey, RateCard: card.Clone(),
			}},
		}},
	}, subscription.Timing{Custom: &periodStart})
	require.NoError(t, err)

	// then: current usage stays 200/1000 and the prior week retains its original pricing.
	items := view.Phases[0].ItemsByKey[featureKey]
	require.Len(t, items, 2)
	require.Equal(t, start, view.Subscription.BillingAnchor)
	require.Equal(t, "hobby", view.Subscription.PlanRef.Key)
	require.Equal(t, "indie", items[1].Spec.RateCard.AsMeta().Metadata["exwave_plan_key"])
	require.Equal(t, periodStart, items[1].SubscriptionItem.ActiveFrom)
	balance, err := deps.EntitlementRegistry.MeteredEntitlement.GetEntitlementBalance(ctx, models.NamespacedID{
		Namespace: view.Subscription.Namespace, ID: items[1].Entitlement.Entitlement.ID,
	}, clock.Now())
	require.NoError(t, err)
	require.Equal(t, 200.0, balance.UsageInPeriod)
	require.Equal(t, 800.0, balance.Balance)
	deps.MockStreamingConnector.AddSimpleEvent(subscriptiontestutils.ExampleFeatureMeterSlug, 1_000, periodStart.Add(time.Hour))
	balance, err = deps.EntitlementRegistry.MeteredEntitlement.GetEntitlementBalance(ctx, models.NamespacedID{
		Namespace: view.Subscription.Namespace, ID: items[1].Entitlement.Entitlement.ID,
	}, clock.Now())
	require.NoError(t, err)
	require.Equal(t, 1_200.0, balance.UsageInPeriod)
	require.Equal(t, 200.0, balance.Overage)

	// when: another upgrade occurs in this same week, its intermediate version is empty.
	card.EntitlementTemplate = productcatalog.NewEntitlementTemplateFrom(productcatalog.MeteredEntitlementTemplate{
		IssueAfterReset: lo.ToPtr(16_000.0), UsagePeriod: week, IsSoftLimit: true,
	})
	card.Discounts.Usage = &productcatalog.UsageDiscount{Quantity: alpacadecimal.NewFromInt(16_000)}
	view, err = deps.WorkflowService.EditRunning(ctx, view.Subscription.NamespacedID, []subscription.Patch{
		patch.PatchAddItem{PhaseKey: phaseKey, ItemKey: featureKey, CreateInput: subscription.SubscriptionItemSpec{
			CreateSubscriptionItemInput: subscription.CreateSubscriptionItemInput{CreateSubscriptionItemPlanInput: subscription.CreateSubscriptionItemPlanInput{
				PhaseKey: phaseKey, ItemKey: featureKey, RateCard: card.Clone(),
			}},
		}},
	}, subscription.Timing{Custom: &periodStart})
	require.NoError(t, err)
	items = view.Phases[0].ItemsByKey[featureKey]
	require.Len(t, items, 3)
	require.Equal(t, items[1].SubscriptionItem.ActiveFrom, *items[1].SubscriptionItem.ActiveTo)
	balance, err = deps.EntitlementRegistry.MeteredEntitlement.GetEntitlementBalance(ctx, models.NamespacedID{
		Namespace: view.Subscription.Namespace, ID: items[2].Entitlement.Entitlement.ID,
	}, clock.Now())
	require.NoError(t, err)
	require.Equal(t, 1_200.0, balance.UsageInPeriod)

	// then: billing's production iterator retains prior pricing and emits one current-week period.
	iterator, err := targetstate.NewPhaseIterator(testutils.NewDiscardLogger(t), noop.NewTracerProvider().Tracer("test"), view, phaseKey)
	require.NoError(t, err)
	periods, err := iterator.Generate(ctx, periodStart.AddDate(0, 0, 7))
	require.NoError(t, err)
	currentPeriods := 0
	for _, item := range periods {
		require.False(t, item.ServicePeriod.IsEmpty())
		switch {
		case item.ServicePeriod.From.Equal(start):
			require.Equal(t, 200.0, item.Spec.RateCard.AsMeta().Discounts.Usage.Quantity.InexactFloat64())
		case item.ServicePeriod.From.Equal(periodStart):
			currentPeriods++
			require.Equal(t, 16_000.0, item.Spec.RateCard.AsMeta().Discounts.Usage.Quantity.InexactFloat64())
		}
	}
	require.Equal(t, 1, currentPeriods)
}

func TestInitialWeekUpgradeThenLaterWeekUpgradePreservesEmptyHistory(t *testing.T) {
	// given: Hobby is upgraded in its first week, leaving a never-active historical version.
	start := time.Date(2025, 7, 16, 0, 0, 13, 0, time.UTC)
	clock.FreezeTime(start)
	defer clock.UnFreeze()
	ctx := t.Context()
	dbDeps := subscriptiontestutils.SetupDBDeps(t)
	defer dbDeps.Cleanup(t)
	deps := subscriptiontestutils.NewService(t, dbDeps)
	deps.FeatureConnector.CreateExampleFeatures(t, deps.ExampleMeterID)
	customer := deps.CustomerAdapter.CreateExampleCustomer(t)
	week := datetime.MustParseDuration(t, "P1W")
	card := subscriptiontestutils.ExampleRateCard1
	card.BillingCadence = week
	card.EntitlementTemplate = productcatalog.NewEntitlementTemplateFrom(productcatalog.MeteredEntitlementTemplate{
		IssueAfterReset: lo.ToPtr(200.0), UsagePeriod: week, IsSoftLimit: true,
	})
	card.Discounts = productcatalog.Discounts{Usage: &productcatalog.UsageDiscount{Quantity: alpacadecimal.NewFromInt(200)}}
	card.Price = productcatalog.NewPriceFrom(productcatalog.UnitPrice{Amount: alpacadecimal.NewFromFloat(0.01)})
	input := subscriptiontestutils.BuildTestPlanInput(t).AddPhase(nil, card.Clone()).Build()
	input.Key = "hobby"
	input.BillingCadence = week
	plan := deps.PlanHelper.CreatePlan(t, input)
	view, err := deps.WorkflowService.CreateFromPlan(ctx, subscriptionworkflow.CreateSubscriptionWorkflowInput{
		Namespace: subscriptiontestutils.ExampleNamespace, CustomerID: customer.ID,
		ChangeSubscriptionWorkflowInput: subscriptionworkflow.ChangeSubscriptionWorkflowInput{Timing: subscription.Timing{Custom: &start}},
	}, plan)
	require.NoError(t, err)
	phaseKey := view.Phases[0].SubscriptionPhase.Key
	featureKey := subscriptiontestutils.ExampleFeatureKey
	laterWeek := start.AddDate(0, 0, 28)
	deps.MockStreamingConnector.AddSimpleEvent(subscriptiontestutils.ExampleFeatureMeterSlug, 1_500, start)
	deps.MockStreamingConnector.AddSimpleEvent(subscriptiontestutils.ExampleFeatureMeterSlug, 200, laterWeek)

	// when: Indie replaces the first week's card, then Studio replaces a later week's card.
	for _, upgrade := range []struct {
		planKey     string
		budget      int64
		periodStart time.Time
	}{
		{planKey: "indie", budget: 1_000, periodStart: start},
		{planKey: "studio", budget: 16_000, periodStart: laterWeek},
	} {
		clock.FreezeTime(upgrade.periodStart.AddDate(0, 0, 2))
		defer clock.UnFreeze()
		card.EntitlementTemplate = productcatalog.NewEntitlementTemplateFrom(productcatalog.MeteredEntitlementTemplate{
			IssueAfterReset: lo.ToPtr(float64(upgrade.budget)), UsagePeriod: week, IsSoftLimit: true,
		})
		card.Discounts.Usage = &productcatalog.UsageDiscount{Quantity: alpacadecimal.NewFromInt(upgrade.budget)}
		card.Metadata = models.Metadata{"exwave_plan_key": upgrade.planKey}
		view, err = deps.WorkflowService.EditRunning(ctx, view.Subscription.NamespacedID, []subscription.Patch{
			patch.PatchAddItem{PhaseKey: phaseKey, ItemKey: featureKey, CreateInput: subscription.SubscriptionItemSpec{
				CreateSubscriptionItemInput: subscription.CreateSubscriptionItemInput{CreateSubscriptionItemPlanInput: subscription.CreateSubscriptionItemPlanInput{
					PhaseKey: phaseKey, ItemKey: featureKey, RateCard: card.Clone(),
				}},
			}},
		}, subscription.Timing{Custom: &upgrade.periodStart})
		require.NoError(t, err)
		require.Equal(t, start, view.Subscription.BillingAnchor)
	}

	// then: reload preserves empty history, the original anchor and current-week consumption.
	view, err = deps.SubscriptionService.GetView(ctx, view.Subscription.NamespacedID)
	require.NoError(t, err)
	items := view.Phases[0].ItemsByKey[featureKey]
	require.Len(t, items, 3)
	require.Equal(t, start, items[0].SubscriptionItem.ActiveFrom)
	require.Equal(t, start, *items[0].SubscriptionItem.ActiveTo)
	require.Equal(t, "indie", items[1].Spec.RateCard.AsMeta().Metadata["exwave_plan_key"])
	require.Equal(t, laterWeek, *items[1].SubscriptionItem.ActiveTo)
	require.Equal(t, "studio", items[2].Spec.RateCard.AsMeta().Metadata["exwave_plan_key"])
	current, err := deps.EntitlementRegistry.MeteredEntitlement.GetEntitlementBalance(ctx, models.NamespacedID{
		Namespace: view.Subscription.Namespace, ID: items[2].Entitlement.Entitlement.ID,
	}, clock.Now())
	require.NoError(t, err)
	require.Equal(t, 200.0, current.UsageInPeriod)
	require.Equal(t, 15_800.0, current.Balance)
	require.Equal(t, 0.0, current.Overage)

	// Billing's production iterator retains four Indie weeks and one Studio week, without an empty Hobby charge.
	iterator, err := targetstate.NewPhaseIterator(testutils.NewDiscardLogger(t), noop.NewTracerProvider().Tracer("test"), view, phaseKey)
	require.NoError(t, err)
	periods, err := iterator.Generate(ctx, laterWeek.AddDate(0, 0, 7))
	require.NoError(t, err)
	require.Len(t, periods, 5)
	for _, item := range periods {
		require.False(t, item.ServicePeriod.IsEmpty())
		if item.ServicePeriod.From.Before(laterWeek) {
			require.Equal(t, 1_000.0, item.Spec.RateCard.AsMeta().Discounts.Usage.Quantity.InexactFloat64())
		} else {
			require.Equal(t, laterWeek, item.ServicePeriod.From)
			require.Equal(t, 16_000.0, item.Spec.RateCard.AsMeta().Discounts.Usage.Quantity.InexactFloat64())
		}
	}
}

func TestDelayedCurrentPeriodPlanChangeUsesExactEffectiveBoundary(t *testing.T) {
	// given: an earlier closed week and a reset webhook delayed by five minutes.
	start := time.Date(2026, 1, 5, 12, 0, 13, 0, time.UTC)
	clock.FreezeTime(start)
	defer clock.UnFreeze()
	ctx := t.Context()
	dbDeps := subscriptiontestutils.SetupDBDeps(t)
	defer dbDeps.Cleanup(t)
	deps := subscriptiontestutils.NewService(t, dbDeps)
	deps.FeatureConnector.CreateExampleFeatures(t, deps.ExampleMeterID)
	customer := deps.CustomerAdapter.CreateExampleCustomer(t)
	week := datetime.MustParseDuration(t, "P1W")
	card := subscriptiontestutils.ExampleRateCard1
	card.BillingCadence = week
	card.EntitlementTemplate = productcatalog.NewEntitlementTemplateFrom(productcatalog.MeteredEntitlementTemplate{
		IssueAfterReset: lo.ToPtr(200.0), UsagePeriod: week, IsSoftLimit: true,
	})
	card.Discounts = productcatalog.Discounts{Usage: &productcatalog.UsageDiscount{Quantity: alpacadecimal.NewFromInt(200)}}
	card.Price = productcatalog.NewPriceFrom(productcatalog.UnitPrice{Amount: alpacadecimal.NewFromFloat(0.01)})
	input := subscriptiontestutils.BuildTestPlanInput(t).AddPhase(nil, card.Clone()).Build()
	input.Key = "hobby"
	input.BillingCadence = week
	hobby := deps.PlanHelper.CreatePlan(t, input)
	view, err := deps.WorkflowService.CreateFromPlan(ctx, subscriptionworkflow.CreateSubscriptionWorkflowInput{
		Namespace: subscriptiontestutils.ExampleNamespace, CustomerID: customer.ID,
		ChangeSubscriptionWorkflowInput: subscriptionworkflow.ChangeSubscriptionWorkflowInput{Timing: subscription.Timing{Custom: &start}},
	}, hobby)
	require.NoError(t, err)
	phaseKey := view.Phases[0].SubscriptionPhase.Key
	periodStart := start.AddDate(0, 0, 7)
	effectiveAt := periodStart.AddDate(0, 0, 2)
	deps.MockStreamingConnector.AddSimpleEvent(subscriptiontestutils.ExampleFeatureMeterSlug, 230, start)
	deps.MockStreamingConnector.AddSimpleEvent(subscriptiontestutils.ExampleFeatureMeterSlug, 80, periodStart)
	clock.FreezeTime(effectiveAt.Add(5*time.Minute + 500*time.Millisecond))
	defer clock.UnFreeze()

	card.EntitlementTemplate = productcatalog.NewEntitlementTemplateFrom(productcatalog.MeteredEntitlementTemplate{
		IssueAfterReset: lo.ToPtr(1_000.0), UsagePeriod: week, IsSoftLimit: true,
	})
	card.Discounts.Usage = &productcatalog.UsageDiscount{Quantity: alpacadecimal.NewFromInt(1_000)}
	input = subscriptiontestutils.BuildTestPlanInput(t).AddPhase(nil, card.Clone()).Build()
	input.Key = "indie"
	input.BillingCadence = week
	indie := deps.PlanHelper.CreatePlan(t, input)

	// Ordinary create and cancellation keep their existing timing constraints.
	_, err = deps.SubscriptionService.Cancel(ctx, view.Subscription.NamespacedID, subscription.Timing{Custom: &effectiveAt})
	require.Error(t, err)
	_, err = deps.WorkflowService.CreateFromPlan(ctx, subscriptionworkflow.CreateSubscriptionWorkflowInput{
		Namespace: subscriptiontestutils.ExampleNamespace, CustomerID: customer.ID,
		ChangeSubscriptionWorkflowInput: subscriptionworkflow.ChangeSubscriptionWorkflowInput{Timing: subscription.Timing{Custom: &effectiveAt}},
	}, indie)
	require.Error(t, err)

	// when: the plan change explicitly supplies the same effective time and new anchor.
	previous, replacement, err := deps.WorkflowService.ChangeToPlan(ctx, view.Subscription.NamespacedID,
		subscriptionworkflow.ChangeSubscriptionWorkflowInput{
			Timing: subscription.Timing{Custom: &effectiveAt}, BillingAnchor: &effectiveAt,
		}, indie)
	require.NoError(t, err)

	// then: raw week-start usage belongs to the replacement even though processing was late.
	require.Equal(t, effectiveAt, *previous.ActiveTo)
	require.Equal(t, effectiveAt, replacement.Subscription.ActiveFrom)
	require.Equal(t, effectiveAt, replacement.Subscription.BillingAnchor)
	require.Equal(t, effectiveAt, replacement.Phases[0].SubscriptionPhase.ActiveFrom)
	deps.MockStreamingConnector.AddSimpleEvent(subscriptiontestutils.ExampleFeatureMeterSlug, 1_200, effectiveAt)
	items := replacement.Phases[0].ItemsByKey[subscriptiontestutils.ExampleFeatureKey]
	balance, err := deps.EntitlementRegistry.MeteredEntitlement.GetEntitlementBalance(ctx, models.NamespacedID{
		Namespace: replacement.Subscription.Namespace, ID: items[0].Entitlement.Entitlement.ID,
	}, clock.Now())
	require.NoError(t, err)
	require.Equal(t, 1_200.0, balance.UsageInPeriod)
	require.Equal(t, 200.0, balance.Overage)

	// The production billing iterator still uses Hobby pricing for the earlier closed week.
	previousView, err := deps.SubscriptionService.GetView(ctx, previous.NamespacedID)
	require.NoError(t, err)
	iterator, err := targetstate.NewPhaseIterator(testutils.NewDiscardLogger(t), noop.NewTracerProvider().Tracer("test"), previousView, phaseKey)
	require.NoError(t, err)
	periods, err := iterator.Generate(ctx, effectiveAt)
	require.NoError(t, err)
	closedPeriods := 0
	for _, item := range periods {
		require.False(t, item.ServicePeriod.IsEmpty())
		if item.ServicePeriod.From.Equal(start) {
			closedPeriods++
			require.Equal(t, periodStart, item.ServicePeriod.To)
			require.Equal(t, 200.0, item.Spec.RateCard.AsMeta().Discounts.Usage.Quantity.InexactFloat64())
		}
	}
	require.Equal(t, 1, closedPeriods)
}
