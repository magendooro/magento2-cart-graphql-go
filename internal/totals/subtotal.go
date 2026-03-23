package totals

import "context"

// SubtotalCollector computes the cart subtotal from item row totals.
// Sort order: 100 (runs first).
type SubtotalCollector struct{}

func (c *SubtotalCollector) Code() string { return "subtotal" }

func (c *SubtotalCollector) Collect(_ context.Context, cc *CollectorContext, total *Total) error {
	for _, item := range cc.Items {
		if item.ParentItemID == nil {
			total.Subtotal += item.RowTotal
		}
	}
	return nil
}
