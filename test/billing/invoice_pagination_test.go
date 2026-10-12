package billing

import (
	"fmt"
	"slices"
	"time"

	"github.com/invopop/gobl/currency"
	"github.com/stretchr/testify/require"

	"github.com/openmeterio/openmeter/api"
	"github.com/openmeterio/openmeter/openmeter/billing"
	"github.com/openmeterio/openmeter/pkg/clock"
	"github.com/openmeterio/openmeter/pkg/currencyx"
	"github.com/openmeterio/openmeter/pkg/filter"
	"github.com/openmeterio/openmeter/pkg/pagination"
	"github.com/openmeterio/openmeter/pkg/sortx"
)

func (s *InvoicingTestSuite) TestListInvoicesEqualCreatedAtPagination() {
	t := s.T()
	ctx := t.Context()
	namespace := s.GetUniqueNamespace("ns-invoice-pagination")
	now := time.Date(2025, time.January, 1, 12, 0, 13, 0, time.UTC)
	clock.FreezeTime(now)
	defer clock.UnFreeze()

	// Given more than two pages of invoices with the same creation timestamp.
	sandboxApp := s.InstallSandboxApp(t, namespace)
	profile := s.ProvisionBillingProfile(ctx, namespace, sandboxApp.GetID())
	customer := s.CreateTestCustomer(namespace, "invoice-pagination")
	const invoiceCount = 205
	ids := make([]string, 0, invoiceCount)
	for i := range invoiceCount {
		status := billing.StandardInvoiceStatusDraftCreated
		if i == 0 {
			status = billing.StandardInvoiceStatusDraftSyncFailed
		}
		invoice, err := s.BillingAdapter.CreateInvoice(ctx, billing.CreateInvoiceAdapterInput{
			Namespace: namespace,
			Customer:  *customer,
			Profile:   *profile,
			Number:    fmt.Sprintf("pagination-%d", i),
			Currency:  currencyx.FiatCode(currency.USD),
			Status:    status,
			IssuedAt:  now,
			Type:      billing.InvoiceTypeStandard,
		})
		require.NoError(t, err)
		require.True(t, invoice.CreatedAt.Equal(now))
		ids = append(ids, invoice.ID)
	}
	failedID := ids[0]
	slices.Sort(ids)

	for _, order := range []sortx.Order{sortx.OrderAsc, sortx.OrderDesc} {
		s.Run(order.String(), func() {
			t := s.T()
			expected := slices.Clone(ids)
			if order == sortx.OrderDesc {
				slices.Reverse(expected)
				require.Contains(t, expected[200:], failedID, "the failed invoice is beyond page one")
			}

			// When the same unchanged result set is read through repeated offset pages.
			for range 2 {
				actual := make([]string, 0, invoiceCount)
				for page := 1; page <= 4; page++ {
					result, err := s.BillingService.ListInvoices(ctx, billing.ListInvoicesInput{
						Page:       pagination.NewPage(page, 100),
						Namespace:  namespace,
						CustomerID: &filter.FilterULID{FilterString: filter.FilterString{Eq: &customer.ID}},
						OrderBy:    api.InvoiceOrderByCreatedAt,
						Order:      order,
						Expand:     billing.InvoiceExpands{billing.InvoiceExpandLines},
					})
					require.NoError(t, err)
					require.Equal(t, invoiceCount, result.TotalCount)
					for _, invoice := range result.Items {
						standard, err := invoice.AsStandardInvoice()
						require.NoError(t, err)
						require.True(t, standard.CreatedAt.Equal(now))
						actual = append(actual, standard.ID)
					}
				}

				// Then every invoice appears once in primary order with a unique ID tie-breaker.
				require.Equal(t, expected, actual)
			}
		})
	}
}
