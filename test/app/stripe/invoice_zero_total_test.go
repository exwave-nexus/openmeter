package appstripe

import (
	"context"
	"time"

	"github.com/alpacahq/alpacadecimal"
	"github.com/invopop/gobl/currency"
	"github.com/samber/lo"
	"github.com/stretchr/testify/mock"
	"github.com/stripe/stripe-go/v80"

	"github.com/openmeterio/openmeter/openmeter/app"
	appstripe "github.com/openmeterio/openmeter/openmeter/app/stripe"
	stripeclient "github.com/openmeterio/openmeter/openmeter/app/stripe/client"
	"github.com/openmeterio/openmeter/openmeter/billing"
	billingservice "github.com/openmeterio/openmeter/openmeter/billing/service"
	"github.com/openmeterio/openmeter/openmeter/customer"
	"github.com/openmeterio/openmeter/openmeter/productcatalog"
	"github.com/openmeterio/openmeter/pkg/clock"
	"github.com/openmeterio/openmeter/pkg/currencyx"
	"github.com/openmeterio/openmeter/pkg/datetime"
	"github.com/openmeterio/openmeter/pkg/models"
	"github.com/openmeterio/openmeter/pkg/timeutil"
	billingtest "github.com/openmeterio/openmeter/test/billing"
)

// Exwave fork: with SkipZeroTotalInvoices, a weekly invoice that stays within
// the usage discount (the weekly grant) is settled inside OpenMeter and never
// reaches Stripe. Weeks with overage keep the standard Stripe flow.

const (
	zeroTotalTestGrantUnits   = 100
	zeroTotalTestStripeNumber = "EXW-0001"
)

type zeroTotalInvoiceFixture struct {
	billing      billing.Service
	app          app.App
	customer     *customer.Customer
	customerData appstripe.CustomerData
	featureKey   string
	periodStart  time.Time
	periodEnd    time.Time
	cleanup      func()
}

// setupZeroTotalInvoiceFixture mirrors Exwave's paid plans: a unit-priced
// weekly compute line whose usage discount equals the weekly grant, and the
// production billing profile (collection PT2H, auto-advance, no draft period,
// charge automatically). Extra profile options apply after these defaults.
func (s *StripeInvoiceTestSuite) setupZeroTotalInvoiceFixture(ctx context.Context, namespace string, profileOpts ...billingtest.BillingProfileProvisionOption) zeroTotalInvoiceFixture {
	periodStart := lo.Must(time.Parse(time.RFC3339, "2026-11-16T00:00:00Z"))
	periodEnd := periodStart.Add(7 * 24 * time.Hour)
	clock.FreezeTime(periodStart)

	_ = s.InstallSandboxApp(s.T(), namespace)

	computeFeature := s.SetupApiRequestsTotalFeature(ctx, namespace)

	customerEntity, err := s.CustomerService.CreateCustomer(ctx, customer.CreateCustomerInput{
		Namespace: namespace,
		CustomerMutate: customer.CustomerMutate{
			Name:     "Paid project",
			Currency: lo.ToPtr(currencyx.Code(currency.EUR)),
			UsageAttribution: &customer.CustomerUsageAttribution{
				SubjectKeys: []string{namespace},
			},
		},
	})
	s.Require().NoError(err)

	stripeApp, err := s.Fixture.setupApp(ctx, namespace)
	s.Require().NoError(err)

	s.ProvisionBillingProfile(ctx, namespace, stripeApp.GetID(), append([]billingtest.BillingProfileProvisionOption{
		billingtest.WithCollectionInterval(datetime.MustParseDuration(s.T(), "PT2H")),
		billingtest.WithBillingProfileEditFn(func(profile *billing.CreateProfileInput) {
			profile.WorkflowConfig.Invoicing.AutoAdvance = true
			profile.WorkflowConfig.Invoicing.DraftPeriod = datetime.MustParseDuration(s.T(), "P0D")
			profile.WorkflowConfig.Invoicing.DueAfter = datetime.MustParseDuration(s.T(), "P0D")
			profile.WorkflowConfig.Payment.CollectionMethod = billing.CollectionMethodChargeAutomatically
		}),
	}, profileOpts...)...)
	s.ProvisionProviderDefaultTaxCode(ctx, namespace)

	customerData, err := s.Fixture.setupAppCustomerData(ctx, stripeApp, customerEntity)
	s.Require().NoError(err)

	svc, ok := s.BillingService.(*billingservice.Service)
	s.Require().True(ok, "billing service must be the concrete service")

	pendingLines, err := s.BillingService.CreatePendingInvoiceLines(ctx, billing.CreatePendingInvoiceLinesInput{
		Customer: customerEntity.GetID(),
		Currency: currencyx.FiatCode(currency.EUR),
		Lines: billing.NewCreatePendingInvoiceLines([]billing.GatheringLine{
			{
				GatheringLineBase: billing.GatheringLineBase{
					ManagedResource: models.NewManagedResource(models.ManagedResourceInput{
						Namespace: namespace,
						Name:      "Compute credits",
					}),
					ServicePeriod: timeutil.ClosedPeriod{From: periodStart, To: periodEnd},
					InvoiceAt:     periodEnd,
					ManagedBy:     billing.ManuallyManagedLine,
					FeatureKey:    computeFeature.Feature.Key,
					Price: lo.FromPtr(productcatalog.NewPriceFrom(productcatalog.UnitPrice{
						Amount: alpacadecimal.NewFromInt(1),
					})),
					RateCardDiscounts: billing.Discounts{
						Usage: &billing.UsageDiscount{
							UsageDiscount: productcatalog.UsageDiscount{
								Quantity: alpacadecimal.NewFromInt(zeroTotalTestGrantUnits),
							},
						},
					},
				},
			},
		}),
	})
	s.Require().NoError(err)
	s.Require().Len(pendingLines.Lines, 1)

	return zeroTotalInvoiceFixture{
		billing:      svc.WithSkipZeroTotalInvoices(true),
		app:          stripeApp,
		customer:     customerEntity,
		customerData: customerData,
		featureKey:   computeFeature.Feature.Key,
		periodStart:  periodStart,
		periodEnd:    periodEnd,
		cleanup: func() {
			computeFeature.Cleanup()
			clock.UnFreeze()
		},
	}
}

func (f zeroTotalInvoiceFixture) addUsage(s *StripeInvoiceTestSuite, units float64, at time.Time) {
	s.MockStreamingConnector.AddSimpleEvent(f.featureKey, units, at)
}

// createWeeklyInvoice invoices the week right after it ends. The invoice then
// waits for the PT2H collection window, as in production.
func (f zeroTotalInvoiceFixture) createWeeklyInvoice(ctx context.Context, s *StripeInvoiceTestSuite) billing.StandardInvoice {
	clock.FreezeTime(f.periodEnd.Add(time.Minute))

	invoices, err := f.billing.InvoicePendingLines(ctx, billing.InvoicePendingLinesInput{
		Customer: f.customer.GetID(),
		AsOf:     lo.ToPtr(f.periodEnd),
	})
	s.Require().NoError(err)
	s.Require().Len(invoices, 1)

	invoice := invoices[0]
	s.Equal(billing.StandardInvoiceStatusDraftWaitingForCollection, invoice.Status)
	s.Empty(invoice.ExternalIDs.Invoicing)

	return invoice
}

// collect advances the invoice once its collection window has passed.
func (f zeroTotalInvoiceFixture) collect(ctx context.Context, s *StripeInvoiceTestSuite, invoice billing.StandardInvoice) billing.StandardInvoice {
	s.Require().NotNil(invoice.CollectionAt)
	clock.FreezeTime(invoice.CollectionAt.Add(time.Minute))

	invoice, err := f.billing.AdvanceInvoice(ctx, invoice.GetInvoiceID())
	s.Require().NoError(err)

	return invoice
}

// expectStripeInvoiceLifecycle mocks the Stripe calls of a standard invoice:
// customer validation, draft creation with its lines, the issuing update and
// finalization. It returns the amounts of every line added to Stripe.
func (f zeroTotalInvoiceFixture) expectStripeInvoiceLifecycle(s *StripeInvoiceTestSuite, stripeInvoiceID string) *[]int64 {
	stripeCustomerID := f.customerData.StripeCustomerID
	stripeInvoice := func() *stripe.Invoice {
		return &stripe.Invoice{
			ID:       stripeInvoiceID,
			Customer: &stripe.Customer{ID: stripeCustomerID},
			Currency: "EUR",
			Lines:    &stripe.InvoiceLineItemList{Data: []*stripe.InvoiceLineItem{}},
		}
	}

	s.StripeAppClient.
		On("GetCustomer", stripeCustomerID).
		Return(stripeclient.StripeCustomer{
			StripeCustomerID: stripeCustomerID,
			DefaultPaymentMethod: &stripeclient.StripePaymentMethod{
				ID:    "pm_123",
				Name:  "Paid project",
				Email: "billing@example.com",
				BillingAddress: &models.Address{
					City:       lo.ToPtr("Lyon"),
					PostalCode: lo.ToPtr("69001"),
					Country:    lo.ToPtr(models.CountryCode("FR")),
					Line1:      lo.ToPtr("1 rue de la République"),
				},
			},
		}, nil)

	s.StripeAppClient.
		On("CreateInvoice", mock.MatchedBy(func(input stripeclient.CreateInvoiceInput) bool {
			return input.AppID == f.app.GetID() &&
				input.CustomerID == f.customer.GetID() &&
				input.StripeCustomerID == stripeCustomerID &&
				input.Currency == currencyx.FiatCode(currency.EUR) &&
				input.CollectionMethod == billing.CollectionMethodChargeAutomatically
		})).
		Once().
		Return(stripeInvoice(), nil)

	addedAmounts := &[]int64{}
	s.StripeAppClient.
		On("AddInvoiceLines", mock.MatchedBy(func(input stripeclient.AddInvoiceLinesInput) bool {
			return input.StripeInvoiceID == stripeInvoiceID
		})).
		Run(func(args mock.Arguments) {
			for _, line := range args.Get(0).(stripeclient.AddInvoiceLinesInput).Lines {
				*addedAmounts = append(*addedAmounts, lo.FromPtr(line.Amount))
			}
		}).
		Return([]stripeclient.StripeInvoiceItemWithLineID{}, nil)

	s.StripeAppClient.
		On("UpdateInvoice", stripeclient.UpdateInvoiceInput{
			AutomaticTaxEnabled: true,
			StripeInvoiceID:     stripeInvoiceID,
		}).
		Return(stripeInvoice(), nil)

	s.StripeAppClient.
		On("ListInvoiceLineItems", stripeInvoiceID).
		Return([]*stripe.InvoiceLineItem{}, nil)

	finalized := stripeInvoice()
	finalized.Number = zeroTotalTestStripeNumber
	s.StripeAppClient.
		On("FinalizeInvoice", stripeclient.FinalizeInvoiceInput{
			AutoAdvance:     true,
			StripeInvoiceID: stripeInvoiceID,
		}).
		Once().
		Return(finalized, nil)

	return addedAmounts
}

func (s *StripeInvoiceTestSuite) stripeAppCallsSince(start int) []string {
	return lo.Map(s.StripeAppClient.Calls[start:], func(call mock.Call, _ int) string {
		return call.Method
	})
}

func (s *StripeInvoiceTestSuite) TestZeroTotalWeekIsSettledWithoutStripe() {
	ctx := context.Background()
	f := s.setupZeroTotalInvoiceFixture(ctx, "ns-zero-total-week")
	defer f.cleanup()

	// Given a paid project that stays within its weekly grant.
	f.addUsage(s, 60, f.periodStart.Add(time.Hour))
	f.addUsage(s, 40, f.periodEnd.Add(-time.Hour))

	// No Stripe client expectations are set: any Stripe call fails the test.
	stripeCallsBefore := len(s.StripeAppClient.Calls)

	// When the week is invoiced and collected.
	invoice := f.createWeeklyInvoice(ctx, s)
	s.True(invoice.Totals.IsZero(), "totals before collection: %+v", invoice.Totals)

	invoice = f.collect(ctx, s, invoice)

	// Then OpenMeter settles it as paid without the invoicing app.
	s.Equal(billing.StandardInvoiceStatusPaid, invoice.Status)
	s.Empty(invoice.ValidationIssues)
	s.False(invoice.StatusDetails.Failed)
	s.True(invoice.StatusDetails.Immutable)
	s.True(invoice.Totals.IsZero(), "totals: %+v", invoice.Totals)
	s.Empty(invoice.ExternalIDs.Invoicing)
	s.Empty(invoice.ExternalIDs.Payment)
	s.NotNil(invoice.IssuedAt)
	s.NotNil(invoice.PaymentProcessingEnteredAt)
	s.Nil(invoice.SentToCustomerAt)
	s.Nil(invoice.DeletedAt)
	s.Empty(s.stripeAppCallsSince(stripeCallsBefore), "no Stripe call is expected for a zero-total week")

	// And the usage line is kept, so subscription sync sees the same invoice.
	lines := invoice.Lines.OrEmpty()
	s.Require().Len(lines, 1)
	s.Nil(lines[0].DeletedAt)
	s.Require().NotNil(lines[0].UsageBased)
	s.Require().NotNil(lines[0].UsageBased.MeteredQuantity)
	s.Equal(float64(100), lines[0].UsageBased.MeteredQuantity.InexactFloat64())
	s.Require().NotNil(lines[0].UsageBased.Quantity)
	s.True(lines[0].UsageBased.Quantity.IsZero(), "the grant covers the whole metered quantity")
	s.True(lines[0].Totals.IsZero())

	// And the persisted invoice matches.
	stored, err := f.billing.GetStandardInvoiceById(ctx, billing.GetStandardInvoiceByIdInput{
		Invoice: invoice.GetInvoiceID(),
		Expand:  billing.StandardInvoiceExpandAll,
	})
	s.Require().NoError(err)
	s.Equal(billing.StandardInvoiceStatusPaid, stored.Status)
	s.Empty(stored.ExternalIDs.Invoicing)
	s.Len(stored.Lines.OrEmpty().WithoutDeletedLines(), 1)
}

func (s *StripeInvoiceTestSuite) TestOverageWeekIsInvoicedInStripe() {
	ctx := context.Background()
	f := s.setupZeroTotalInvoiceFixture(ctx, "ns-zero-total-overage")
	defer f.cleanup()

	// Given a paid project that uses 60 units more than its weekly grant.
	f.addUsage(s, 160, f.periodStart.Add(time.Hour))

	invoice := f.createWeeklyInvoice(ctx, s)

	// When the week is collected.
	addedAmounts := f.expectStripeInvoiceLifecycle(s, "in_overage")
	invoice = f.collect(ctx, s, invoice)

	// Then the invoice is created and finalized in Stripe as before, and waits
	// for Stripe's payment.
	s.Equal(billing.StandardInvoiceStatusPaymentProcessingPending, invoice.Status)
	s.Empty(invoice.ValidationIssues)
	s.Equal("in_overage", invoice.ExternalIDs.Invoicing)
	s.Equal(zeroTotalTestStripeNumber, invoice.Number)
	s.Equal(float64(60), invoice.Totals.Total.InexactFloat64())
	s.Require().NotEmpty(*addedAmounts)
	s.Equal(int64(6000), (*addedAmounts)[0])
	s.StripeAppClient.AssertExpectations(s.T())

	// When Stripe reports the payment, the invoice is paid.
	err := f.billing.TriggerInvoice(ctx, billing.InvoiceTriggerServiceInput{
		InvoiceTriggerInput: billing.InvoiceTriggerInput{
			Invoice: invoice.GetInvoiceID(),
			Trigger: billing.TriggerPaid,
		},
		AppType:    app.AppTypeStripe,
		Capability: app.CapabilityTypeCollectPayments,
	})
	s.Require().NoError(err)

	paid, err := f.billing.GetStandardInvoiceById(ctx, billing.GetStandardInvoiceByIdInput{
		Invoice: invoice.GetInvoiceID(),
	})
	s.Require().NoError(err)
	s.Equal(billing.StandardInvoiceStatusPaid, paid.Status)
}

func (s *StripeInvoiceTestSuite) TestWeekBecomingNonZeroBeforeCollectionIsInvoicedInStripe() {
	ctx := context.Background()
	f := s.setupZeroTotalInvoiceFixture(ctx, "ns-zero-total-late-usage")
	defer f.cleanup()

	// Given a week that is within its grant when the invoice is created.
	f.addUsage(s, 60, f.periodStart.Add(time.Hour))

	stripeCallsBefore := len(s.StripeAppClient.Calls)
	invoice := f.createWeeklyInvoice(ctx, s)
	s.True(invoice.Totals.IsZero(), "totals before collection: %+v", invoice.Totals)
	s.Empty(s.stripeAppCallsSince(stripeCallsBefore), "the draft must not reach Stripe before collection")

	// When usage from the end of the week arrives during the collection window.
	f.addUsage(s, 100, f.periodEnd.Add(-time.Hour))

	addedAmounts := f.expectStripeInvoiceLifecycle(s, "in_late_usage")
	invoice = f.collect(ctx, s, invoice)

	// Then the collected overage is invoiced in Stripe as before.
	s.Equal(billing.StandardInvoiceStatusPaymentProcessingPending, invoice.Status)
	s.Empty(invoice.ValidationIssues)
	s.Equal("in_late_usage", invoice.ExternalIDs.Invoicing)
	s.Equal(zeroTotalTestStripeNumber, invoice.Number)
	s.Equal(float64(60), invoice.Totals.Total.InexactFloat64())
	s.Require().NotEmpty(*addedAmounts)
	s.Equal(int64(6000), (*addedAmounts)[0])
	s.StripeAppClient.AssertExpectations(s.T())
}

func (s *StripeInvoiceTestSuite) TestZeroTotalDraftIsDeletedWithoutStripe() {
	ctx := context.Background()
	f := s.setupZeroTotalInvoiceFixture(ctx, "ns-zero-total-delete", billingtest.WithManualApproval())
	defer f.cleanup()

	// Given a zero-total draft waiting for manual approval: Stripe has no draft for it.
	f.addUsage(s, 50, f.periodStart.Add(time.Hour))

	stripeCallsBefore := len(s.StripeAppClient.Calls)
	invoice := f.collect(ctx, s, f.createWeeklyInvoice(ctx, s))
	s.Equal(billing.StandardInvoiceStatusDraftManualApprovalNeeded, invoice.Status)
	s.Empty(invoice.ExternalIDs.Invoicing)

	// When it is deleted.
	invoice, err := f.billing.DeleteInvoice(ctx, billing.DeleteInvoiceInput{
		Invoice:        invoice.GetInvoiceID(),
		DeletionSource: billing.ChangeSourceAPIRequest,
	})
	s.Require().NoError(err)

	// Then the deletion completes without asking Stripe to delete a missing invoice.
	s.Equal(billing.StandardInvoiceStatusDeleted, invoice.Status)
	s.Empty(invoice.ValidationIssues)
	s.Empty(s.stripeAppCallsSince(stripeCallsBefore), "no Stripe call is expected for a zero-total draft")
}
