package totals

import (
	"context"
	"math"

	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
)

// DiscountCollector applies coupon/salesrule discounts to the cart totals.
// Sort order: 300 (after subtotal, before shipping).
type DiscountCollector struct {
	CouponRepo *repository.CouponRepository
}

func (c *DiscountCollector) Code() string { return "discount" }

func (c *DiscountCollector) Collect(ctx context.Context, cc *CollectorContext, total *Total) error {
	if cc.Quote.CouponCode == nil || *cc.Quote.CouponCode == "" {
		return nil
	}

	// Look up the rule(s) for this coupon
	_, rule, err := c.CouponRepo.LookupCoupon(ctx, *cc.Quote.CouponCode, 1, 0) // website=1, guest group=0
	if err != nil {
		return nil // coupon no longer valid, skip discount silently
	}

	// Get target SKUs (if rule has SKU conditions)
	targetSkus := c.CouponRepo.GetRuleActionSkus(ctx, rule.RuleID)
	skuSet := make(map[string]bool, len(targetSkus))
	for _, s := range targetSkus {
		skuSet[s] = true
	}

	// Apply discount to matching items
	for _, item := range cc.Items {
		if item.ParentItemID != nil {
			continue
		}
		// Check if item matches rule's SKU conditions (nil = all products)
		if len(skuSet) > 0 && !skuSet[item.SKU] {
			continue
		}

		var itemDiscount float64
		switch rule.SimpleAction {
		case "by_percent":
			itemDiscount = item.RowTotal * rule.DiscountAmount / 100.0
		case "by_fixed":
			itemDiscount = rule.DiscountAmount * item.Qty
		case "cart_fixed":
			// Distribute across items proportionally
			if total.Subtotal > 0 {
				itemDiscount = rule.DiscountAmount * (item.RowTotal / total.Subtotal)
			}
		}

		itemDiscount = math.Round(itemDiscount*100) / 100
		if itemDiscount > item.RowTotal {
			itemDiscount = item.RowTotal
		}
		total.DiscountAmount += itemDiscount
	}

	return nil
}
