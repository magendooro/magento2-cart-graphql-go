// Package totals implements a pipeline-based totals calculation system
// inspired by Magento's TotalsCollector. Each collector computes one
// aspect of the cart totals (subtotal, tax, shipping, grand total)
// and mutates a shared Total struct in a defined order.
package totals

import (
	"context"
	"fmt"

	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
)

// Total accumulates all computed values for a quote.
type Total struct {
	Subtotal       float64
	DiscountAmount float64
	ShippingAmount float64
	TaxAmount      float64
	GrandTotal     float64

	// ShippingTaxAmount is the tax on shipping only.
	// Tracked separately so GrandTotalCollector can exclude product tax
	// when price_includes_tax is active (product tax is already in Subtotal).
	ShippingTaxAmount float64

	// TaxIncludedInPrice is true when tax/calculation/price_includes_tax is enabled.
	// In this mode product tax is extracted from prices, not added on top.
	TaxIncludedInPrice bool

	// Per-item tax breakdown (itemID → tax amount)
	ItemTaxes map[int]float64

	// Applied tax labels for display
	AppliedTaxes []AppliedTax
}

// AppliedTax holds a computed tax amount with its label.
type AppliedTax struct {
	Label  string
	Amount float64
	Rate   float64
}

// CollectorContext provides the data collectors need.
type CollectorContext struct {
	Quote              *repository.CartData
	Items              []*repository.CartItemData
	Address            *repository.CartAddressData // shipping address (nil for virtual carts)
	StoreID            int
	CustomerTaxClassID int // resolved from customer_group; 3 (Retail Customer) when unset
}

// Collector computes one aspect of cart totals.
type Collector interface {
	Code() string
	Collect(ctx context.Context, cc *CollectorContext, total *Total) error
}

// Pipeline runs collectors in order.
type Pipeline struct {
	collectors []Collector
}

// NewPipeline creates a totals pipeline with the given collectors.
// Collectors run in the order provided — caller is responsible for sort order.
func NewPipeline(collectors ...Collector) *Pipeline {
	return &Pipeline{collectors: collectors}
}

// Collect runs all collectors in sequence, returning the accumulated totals.
func (p *Pipeline) Collect(ctx context.Context, cc *CollectorContext) (*Total, error) {
	total := &Total{
		ItemTaxes: make(map[int]float64),
	}
	for _, c := range p.collectors {
		if err := c.Collect(ctx, cc, total); err != nil {
			return total, fmt.Errorf("collector %s: %w", c.Code(), err)
		}
	}
	return total, nil
}
