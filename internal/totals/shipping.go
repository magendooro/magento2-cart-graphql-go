package totals

import "context"

// ShippingCollector reads the shipping amount from the selected shipping method.
// Sort order: 350.
type ShippingCollector struct{}

func (c *ShippingCollector) Code() string { return "shipping" }

func (c *ShippingCollector) Collect(_ context.Context, cc *CollectorContext, total *Total) error {
	if cc.Address != nil {
		total.ShippingAmount = cc.Address.ShippingAmount
	}
	return nil
}
