package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/samber/lo"

	"github.com/openmeterio/openmeter/openmeter/billing/worker/subscriptionsync"
	"github.com/openmeterio/openmeter/openmeter/customer"
	"github.com/openmeterio/openmeter/openmeter/subscription"
	"github.com/openmeterio/openmeter/pkg/clock"
	"github.com/openmeterio/openmeter/pkg/filter"
	"github.com/openmeterio/openmeter/pkg/framework/transaction"
	"github.com/openmeterio/openmeter/pkg/models"
	"github.com/openmeterio/openmeter/pkg/pagination"
	"github.com/openmeterio/openmeter/pkg/timeutil"
)

const (
	defaultWindowSize = 10_000
)

// Reconciler is a component that periodically reconciles the subscription state with the billing state
// this is essential, as the invoice creation is purley event driven. In case a processing error happens,
// we might fail to create an invoice, and stop processing the subscription.
type Reconciler struct {
	subscriptionSync    subscriptionsync.Service
	subscriptionService subscription.Service
	customerService     customer.Service
	logger              *slog.Logger
}

type ReconcilerConfig struct {
	SubscriptionSync    subscriptionsync.Service
	SubscriptionService subscription.Service
	CustomerService     customer.Service
	Logger              *slog.Logger
}

func (c ReconcilerConfig) Validate() error {
	if c.SubscriptionSync == nil {
		return errors.New("subscriptionSync is required")
	}

	if c.SubscriptionService == nil {
		return errors.New("subscriptionService is required")
	}

	if c.CustomerService == nil {
		return errors.New("customerService is required")
	}

	if c.Logger == nil {
		return errors.New("logger is required")
	}

	return nil
}

func NewReconciler(config ReconcilerConfig) (*Reconciler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &Reconciler{
		subscriptionSync:    config.SubscriptionSync,
		subscriptionService: config.SubscriptionService,
		customerService:     config.CustomerService,
		logger:              config.Logger,
	}, nil
}

type ReconcilerListSubscriptionsInput struct {
	Namespaces []string
	Customers  []string
	Lookback   time.Duration
}

func (i ReconcilerListSubscriptionsInput) Validate() error {
	if i.Lookback <= 0 {
		return errors.New("lookback must be greater than 0")
	}

	return nil
}

type SubscriptionWithSyncState struct {
	subscription.Subscription
	*subscriptionsync.SyncState
}

func (r *Reconciler) ListSubscriptions(ctx context.Context, in ReconcilerListSubscriptionsInput) ([]SubscriptionWithSyncState, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	pageIndex := 1

	var out []SubscriptionWithSyncState

	var customerID *filter.FilterULID
	if len(in.Customers) > 0 {
		customerID = &filter.FilterULID{FilterString: filter.FilterString{In: &in.Customers}}
	}

	for {
		subscriptions, err := r.subscriptionService.List(ctx, subscription.ListSubscriptionsInput{
			Namespaces: in.Namespaces,
			CustomerID: customerID,
			ActiveInPeriod: &timeutil.StartBoundedPeriod{
				From: clock.Now().Add(-in.Lookback),
				To:   lo.ToPtr(clock.Now()),
			},
			Page: pagination.Page{
				PageNumber: pageIndex,
				PageSize:   defaultWindowSize,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list subscriptions: %w", err)
		}

		if len(subscriptions.Items) == 0 {
			break
		}

		mapped, err := r.mapToSubscriptionWithSyncState(ctx, subscriptions.Items)
		if err != nil {
			return nil, fmt.Errorf("failed to map subscriptions to subscription with sync state: %w", err)
		}

		out = append(out, mapped...)

		pageIndex++
	}

	pageIndex = 1

	for {
		subscriptions, err := r.subscriptionService.List(ctx, subscription.ListSubscriptionsInput{
			Namespaces: in.Namespaces,
			CustomerID: customerID,
			// TODO: Later we might want to have a lookback for deleted subscriptions as well, but as of 2026-05-19
			// we had a delete bug, so we need to reconcile all deleted subscriptions.
			DeletedAt: &filter.FilterTime{
				Lte: lo.ToPtr(clock.Now()),
			},
			IncludeDeleted: true,
			Page: pagination.Page{
				PageNumber: pageIndex,
				PageSize:   defaultWindowSize,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list subscriptions: %w", err)
		}

		if len(subscriptions.Items) == 0 {
			break
		}

		mapped, err := r.mapToSubscriptionWithSyncState(ctx, subscriptions.Items)
		if err != nil {
			return nil, fmt.Errorf("failed to map subscriptions to subscription with sync state: %w", err)
		}

		out = append(out, mapped...)

		pageIndex++
	}

	return out, nil
}

func (r *Reconciler) mapToSubscriptionWithSyncState(ctx context.Context, subs []subscription.Subscription) ([]SubscriptionWithSyncState, error) {
	syncStates, err := r.subscriptionSync.GetSyncStates(ctx, lo.Map(subs, func(item subscription.Subscription, _ int) models.NamespacedID {
		return models.NamespacedID{
			ID:        item.ID,
			Namespace: item.Namespace,
		}
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to get sync states: %w", err)
	}

	syncStatesBySubscriptionID := lo.SliceToMap(syncStates, func(syncState subscriptionsync.SyncState) (models.NamespacedID, subscriptionsync.SyncState) {
		return models.NamespacedID{
			ID:        syncState.SubscriptionID.ID,
			Namespace: syncState.SubscriptionID.Namespace,
		}, syncState
	})

	return lo.Map(subs, func(item subscription.Subscription, _ int) SubscriptionWithSyncState {
		existingSyncState, ok := syncStatesBySubscriptionID[item.NamespacedID]

		var syncState *subscriptionsync.SyncState
		if ok {
			syncState = lo.ToPtr(existingSyncState)
		}

		return SubscriptionWithSyncState{
			Subscription: item,
			SyncState:    syncState,
		}
	}), nil
}

func (r *Reconciler) ReconcileSubscription(ctx context.Context, subsID models.NamespacedID) error {
	return r.subscriptionSync.SyncByID(ctx, subsID, clock.Now(), subscriptionsync.SkipCustomCurrencySubscriptions())
}

type ReconcilerAllInput struct {
	ReconcilerListSubscriptionsInput
	Force bool
	// Concurrency opts into parallel customer reconciliation. Values <= 1
	// preserve the serial path, including the original subscription order.
	Concurrency int
}

func (r *Reconciler) All(ctx context.Context, in ReconcilerAllInput) error {
	subscriptions, err := r.ListSubscriptions(ctx, in.ReconcilerListSubscriptionsInput)
	if err != nil {
		return fmt.Errorf("failed to list subscriptions: %w", err)
	}

	// Nested transactions reuse one savepoint stack and cannot be shared by
	// independent workers. Existing production callers also stay serial.
	_, transactionErr := transaction.GetDriverFromContext(ctx)
	if in.Concurrency <= 1 || transactionErr == nil {
		var outErr error
		for _, subscription := range subscriptions {
			if err := r.reconcileSubscriptionIfNeeded(ctx, subscription, in.Force); err != nil {
				outErr = errors.Join(outErr, err)
			}
		}
		return outErr
	}

	// Keep each customer's subscriptions in their original list order. Billing
	// reconciliation owns customer-wide gathering state under its database lock.
	groupIndexes := make(map[models.NamespacedID]int)
	groups := make([][]int, 0)
	for index, subscription := range subscriptions {
		customerID := models.NamespacedID(subscription.GetCustomerID())
		groupIndex, ok := groupIndexes[customerID]
		if !ok {
			groupIndex = len(groups)
			groupIndexes[customerID] = groupIndex
			groups = append(groups, nil)
		}
		groups[groupIndex] = append(groups[groupIndex], index)
	}

	errs := make([]error, len(subscriptions))
	jobs := make(chan []int)
	var workers sync.WaitGroup
	for range min(in.Concurrency, len(groups)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for indexes := range jobs {
				for _, index := range indexes {
					errs[index] = r.reconcileSubscriptionIfNeeded(ctx, subscriptions[index], in.Force)
				}
			}
		}()
	}
	for _, indexes := range groups {
		jobs <- indexes
	}
	close(jobs)
	workers.Wait()

	// All eligible subscriptions are attempted; report failures in list order
	// regardless of which customer worker completed first.
	return errors.Join(errs...)
}

func (r *Reconciler) reconcileSubscriptionIfNeeded(ctx context.Context, subscription SubscriptionWithSyncState, force bool) error {
	if !force && subscription.SyncState != nil {
		if !subscription.SyncState.HasBillables {
			r.logger.InfoContext(ctx, "subscription has no billables, skipping reconciliation", "subscription_id", subscription.NamespacedID)
			return nil
		}

		if subscription.SyncState.NextSyncAfter == nil {
			r.logger.InfoContext(ctx, "subscription has no next sync after, skipping reconciliation", "subscription_id", subscription.NamespacedID)
			return nil
		}

		if subscription.SyncState.NextSyncAfter.After(clock.Now()) {
			r.logger.InfoContext(ctx, "subscription next sync after is in the future, skipping reconciliation", "subscription_id", subscription.NamespacedID)
			return nil
		}
	}

	if err := r.ReconcileSubscription(ctx, subscription.NamespacedID); err != nil {
		r.logger.ErrorContext(ctx, "failed to reconcile subscription", "error", err)
		return fmt.Errorf("failed to reconcile subscription: %w", err)
	}
	return nil
}
