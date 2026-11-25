package billingservice

import (
	"time"

	"github.com/samber/lo"

	"github.com/openmeterio/openmeter/openmeter/billing"
	"github.com/openmeterio/openmeter/openmeter/billing/service/invoicecalc"
	"github.com/openmeterio/openmeter/pkg/clock"
)

// Exwave fork: zero-total invoices are settled inside OpenMeter.
//
// When the billing service runs with SkipZeroTotalInvoices, an invoice whose
// totals are all zero and that the invoicing app has never received (no
// invoicing external ID) is not validated, synced, finalized or deleted by the
// app. It is issued internally and moves from payment_processing.pending to
// paid on its own, so no provider invoice (such as a numbered Stripe invoice)
// exists for it. The lines are kept, so subscription sync sees the same
// invoice as before.
//
// The decision is re-evaluated on every app operation from the current
// totals: draft syncing (where the Stripe draft would first be created) and
// issuing syncing (after line finalization, so from the final totals). An
// invoice that becomes non-zero before issuing is created and finalized in the
// app exactly as before. An invoice the app has already received keeps the
// standard flow even if its totals later become zero.

// settlesWithoutInvoicingApp reports whether the invoice skips the invoicing app.
func (m *InvoiceStateMachine) settlesWithoutInvoicingApp() bool {
	if m.Service == nil || !m.Service.skipZeroTotalInvoices {
		return false
	}

	return isZeroTotalInvoiceWithoutInvoicingApp(m.Invoice)
}

func isZeroTotalInvoiceWithoutInvoicingApp(invoice billing.StandardInvoice) bool {
	return invoice.ExternalIDs.Invoicing == "" && invoice.Totals.IsZero()
}

// logInvoicingAppSkipped records which app operation a zero-total invoice skipped.
func (m *InvoiceStateMachine) logInvoicingAppSkipped(op billing.StandardInvoiceOperation) {
	if m.Logger == nil {
		return
	}

	m.Logger.Info("zero-total invoice skips the invoicing app",
		"invoice_id", m.Invoice.ID,
		"namespace", m.Invoice.Namespace,
		"status", m.Invoice.Status,
		"operation", op,
	)
}

// issueWithoutInvoicingApp sets the fields that the issuing sync would set,
// without calling the invoicing app.
func (m *InvoiceStateMachine) issueWithoutInvoicingApp() error {
	m.logInvoicingAppSkipped(billing.StandardInvoiceOpFinalize)

	m.Invoice.IssuedAt = lo.ToPtr(clock.Now().In(time.UTC))

	return invoicecalc.CalculateDueAt(&m.Invoice)
}
