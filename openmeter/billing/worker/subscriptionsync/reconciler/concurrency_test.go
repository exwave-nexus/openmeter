package reconciler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openmeterio/openmeter/openmeter/billing/worker/subscriptionsync"
	"github.com/openmeterio/openmeter/openmeter/subscription"
	"github.com/openmeterio/openmeter/openmeter/testutils"
	"github.com/openmeterio/openmeter/pkg/clock"
	"github.com/openmeterio/openmeter/pkg/framework/transaction"
	"github.com/openmeterio/openmeter/pkg/models"
)

func TestAllReconcilesCustomersConcurrentlyInSubscriptionOrder(t *testing.T) {
	// Given interleaved subscriptions, including the same customer ID in two namespaces.
	subs := make([]subscription.Subscription, 0, 24)
	expectedByCustomer := make(map[models.NamespacedID][]models.NamespacedID)
	for version := range 3 {
		for customerIndex := range 8 {
			sub := subscription.Subscription{
				NamespacedID: models.NamespacedID{
					Namespace: fmt.Sprintf("namespace-%d", customerIndex%2),
					ID:        fmt.Sprintf("subscription-%d-%d", customerIndex, version),
				},
				CustomerId: fmt.Sprintf("customer-%d", customerIndex/2),
			}
			subs = append(subs, sub)
			customerID := models.NamespacedID(sub.GetCustomerID())
			expectedByCustomer[customerID] = append(expectedByCustomer[customerID], sub.NamespacedID)
		}
	}
	firstFailure := errors.New("first failed subscription")
	middleFailure := errors.New("middle failed subscription")
	lastFailure := errors.New("last failed subscription")
	release := make(chan struct{})
	spy := &customerReconciliationSpy{
		subscriptions: subs,
		failures: map[models.NamespacedID]error{
			subs[0].NamespacedID:  firstFailure,
			subs[10].NamespacedID: middleFailure,
			subs[17].NamespacedID: lastFailure,
		},
		release: release,
		started: make(chan models.NamespacedID, len(subs)),
	}
	r := Reconciler{
		subscriptionService: &reconciliationSubscriptions{subscriptions: subs},
		subscriptionSync:    spy,
		logger:              testutils.NewDiscardLogger(t),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	completed := make(chan error, 1)

	// When three customer workers reconcile and all individual failures are retained.
	go func() {
		completed <- r.All(ctx, ReconcilerAllInput{
			ReconcilerListSubscriptionsInput: ReconcilerListSubscriptionsInput{Lookback: time.Hour},
			Concurrency:                      3,
		})
	}()
	started := make([]models.NamespacedID, 0, 3)
	for range 3 {
		select {
		case id := <-spy.started:
			started = append(started, id)
		case <-ctx.Done():
			t.Fatal("independent customer workers did not start concurrently")
		}
	}
	require.ElementsMatch(t, []models.NamespacedID{subs[0].NamespacedID, subs[1].NamespacedID, subs[2].NamespacedID}, started)
	close(release)
	var err error
	select {
	case err = <-completed:
	case <-ctx.Done():
		t.Fatal("customer reconciliation did not finish")
	}

	// Then the worker bound and each customer's order hold, and errors keep list order.
	require.ErrorIs(t, err, firstFailure)
	require.ErrorIs(t, err, middleFailure)
	require.ErrorIs(t, err, lastFailure)
	require.Equal(t, "failed to reconcile subscription: first failed subscription\nfailed to reconcile subscription: middle failed subscription\nfailed to reconcile subscription: last failed subscription", err.Error())
	require.Equal(t, 3, spy.peak)
	require.False(t, spy.customerOverlap)
	require.False(t, spy.missingSkipOption)
	require.Len(t, spy.calls, len(subs))
	for customerID, expected := range expectedByCustomer {
		actual := make([]models.NamespacedID, 0, len(expected))
		for _, call := range spy.calls {
			if spy.customerByID(call) == customerID {
				actual = append(actual, call)
			}
		}
		require.Equal(t, expected, actual)
	}
}

func TestAllDefaultAndCallerTransactionKeepExactSerialOrder(t *testing.T) {
	subs := []subscription.Subscription{
		{NamespacedID: models.NamespacedID{Namespace: "namespace", ID: "first"}, CustomerId: "customer-one"},
		{NamespacedID: models.NamespacedID{Namespace: "namespace", ID: "second"}, CustomerId: "customer-two"},
		{NamespacedID: models.NamespacedID{Namespace: "namespace", ID: "third"}, CustomerId: "customer-one"},
	}
	for _, test := range []struct {
		name           string
		concurrency    int
		transactionCtx bool
	}{
		{name: "default"},
		{name: "one worker", concurrency: 1},
		{name: "caller transaction", concurrency: 16, transactionCtx: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Given a serial default or a shared caller-owned transaction.
			ctx := t.Context()
			if test.transactionCtx {
				var err error
				ctx, err = transaction.SetDriverOnContext(ctx, reconciliationTransaction{})
				require.NoError(t, err)
			}
			spy := &customerReconciliationSpy{subscriptions: subs}
			r := Reconciler{
				subscriptionService: &reconciliationSubscriptions{subscriptions: subs},
				subscriptionSync:    spy,
				logger:              testutils.NewDiscardLogger(t),
			}

			// When all subscriptions reconcile, then their original global order remains intact.
			require.NoError(t, r.All(ctx, ReconcilerAllInput{
				ReconcilerListSubscriptionsInput: ReconcilerListSubscriptionsInput{Lookback: time.Hour},
				Concurrency:                      test.concurrency,
			}))
			require.Equal(t, []models.NamespacedID{subs[0].NamespacedID, subs[1].NamespacedID, subs[2].NamespacedID}, spy.calls)
			require.Equal(t, 1, spy.peak)
		})
	}
}

func TestAllConcurrentReconciliationPreservesEligibilityAndForce(t *testing.T) {
	now := time.Date(2025, time.January, 1, 12, 0, 13, 0, time.UTC)
	clock.FreezeTime(now)
	defer clock.UnFreeze()
	before, after := now.Add(-time.Second), now.Add(time.Second)
	subs := make([]subscription.Subscription, 6)
	for i := range subs {
		subs[i] = subscription.Subscription{
			NamespacedID: models.NamespacedID{Namespace: "namespace", ID: fmt.Sprintf("subscription-%d", i)},
			CustomerId:   "customer",
		}
	}
	states := []subscriptionsync.SyncState{
		{SubscriptionID: subs[1].NamespacedID, HasBillables: false},
		{SubscriptionID: subs[2].NamespacedID, HasBillables: true},
		{SubscriptionID: subs[3].NamespacedID, HasBillables: true, NextSyncAfter: &after},
		{SubscriptionID: subs[4].NamespacedID, HasBillables: true, NextSyncAfter: &before},
		{SubscriptionID: subs[5].NamespacedID, HasBillables: true, NextSyncAfter: &now},
	}
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			// Given unknown, unbillable, unscheduled, future, overdue and due sync states.
			spy := &customerReconciliationSpy{subscriptions: subs, states: states}
			r := Reconciler{
				subscriptionService: &reconciliationSubscriptions{subscriptions: subs},
				subscriptionSync:    spy,
				logger:              testutils.NewDiscardLogger(t),
			}

			// When concurrency is enabled, then normal eligibility and forced reconciliation match serial semantics.
			require.NoError(t, r.All(t.Context(), ReconcilerAllInput{
				ReconcilerListSubscriptionsInput: ReconcilerListSubscriptionsInput{Lookback: time.Hour},
				Force:                            force,
				Concurrency:                      16,
			}))
			expected := []models.NamespacedID{subs[0].NamespacedID, subs[4].NamespacedID, subs[5].NamespacedID}
			if force {
				expected = expected[:0]
				for _, sub := range subs {
					expected = append(expected, sub.NamespacedID)
				}
			}
			require.Equal(t, expected, spy.calls)
		})
	}
}

type reconciliationSubscriptions struct {
	subscription.Service
	subscriptions []subscription.Subscription
}

func (s *reconciliationSubscriptions) List(_ context.Context, in subscription.ListSubscriptionsInput) (subscription.SubscriptionList, error) {
	if in.PageNumber != 1 || in.DeletedAt != nil {
		return subscription.SubscriptionList{}, nil
	}
	return subscription.SubscriptionList{Items: slices.Clone(s.subscriptions)}, nil
}

type customerReconciliationSpy struct {
	subscriptionsync.Service
	subscriptions []subscription.Subscription
	states        []subscriptionsync.SyncState
	failures      map[models.NamespacedID]error
	release       <-chan struct{}
	started       chan models.NamespacedID

	mu                sync.Mutex
	calls             []models.NamespacedID
	active            int
	peak              int
	activeCustomers   map[models.NamespacedID]int
	customerOverlap   bool
	missingSkipOption bool
}

func (s *customerReconciliationSpy) GetSyncStates(context.Context, subscriptionsync.GetSyncStatesInput) ([]subscriptionsync.SyncState, error) {
	return slices.Clone(s.states), nil
}

func (s *customerReconciliationSpy) customerByID(id models.NamespacedID) models.NamespacedID {
	for _, sub := range s.subscriptions {
		if sub.NamespacedID == id {
			return models.NamespacedID(sub.GetCustomerID())
		}
	}
	return models.NamespacedID{}
}

func (s *customerReconciliationSpy) SyncByID(ctx context.Context, id models.NamespacedID, _ time.Time, opts ...subscriptionsync.SynchronizeSubscriptionOption) error {
	options := subscriptionsync.SynchronizeSubscriptionOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	customerID := s.customerByID(id)
	s.mu.Lock()
	if s.activeCustomers == nil {
		s.activeCustomers = make(map[models.NamespacedID]int)
	}
	s.calls = append(s.calls, id)
	s.active++
	s.peak = max(s.peak, s.active)
	s.activeCustomers[customerID]++
	s.customerOverlap = s.customerOverlap || s.activeCustomers[customerID] > 1
	s.missingSkipOption = s.missingSkipOption || !options.SkipCustomCurrencySubscriptions
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.active--
		s.activeCustomers[customerID]--
	}()
	if s.started != nil {
		s.started <- id
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.failures[id]
}

type reconciliationTransaction struct{}

func (reconciliationTransaction) Commit() error    { return nil }
func (reconciliationTransaction) Rollback() error  { return nil }
func (reconciliationTransaction) SavePoint() error { return nil }
