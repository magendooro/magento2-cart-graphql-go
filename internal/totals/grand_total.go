package totals

import "context"

// GrandTotalCollector computes the final grand total.
// Sort order: 550 (always runs last).
type GrandTotalCollector struct{}

func (c *GrandTotalCollector) Code() string { return "grand_total" }

func (c *GrandTotalCollector) Collect(_ context.Context, _ *CollectorContext, total *Total) error {
	total.GrandTotal = total.Subtotal - total.DiscountAmount + total.ShippingAmount + total.TaxAmount
	return nil
}
