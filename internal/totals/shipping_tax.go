package totals

import (
	"context"
	"math"

	"github.com/magendooro/magento2-go-common/config"
	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
)

// ShippingTaxCollector applies tax to the shipping amount.
// Sort order: 375 (after shipping, before main tax collector).
//
// Only active when tax/classes/shipping_tax_class > 0 in config.
// Uses the same tax_calculation_rate → tax_calculation logic as product tax.
type ShippingTaxCollector struct {
	TaxRepo *repository.TaxRepository
	CP      *config.ConfigProvider
}

func (c *ShippingTaxCollector) Code() string { return "tax_shipping" }

func (c *ShippingTaxCollector) Collect(ctx context.Context, cc *CollectorContext, total *Total) error {
	if cc.Address == nil || cc.Address.CountryID == "" || total.ShippingAmount == 0 {
		return nil
	}

	// Check if shipping tax class is configured
	shippingTaxClassID := c.CP.GetInt("tax/classes/shipping_tax_class", cc.StoreID, 0)
	if shippingTaxClassID == 0 {
		return nil
	}

	regionID := 0
	if cc.Address.RegionID != nil {
		regionID = *cc.Address.RegionID
	}
	postcode := "*"
	if cc.Address.Postcode != nil {
		postcode = *cc.Address.Postcode
	}

	customerTaxClassID := cc.CustomerTaxClassID
	if customerTaxClassID == 0 {
		customerTaxClassID = 3 // Retail Customer fallback
	}

	// Create a virtual item representing the shipping amount with the shipping tax class
	shippingItem := &repository.CartItemData{
		ItemID:            -1, // virtual item
		RowTotal:          total.ShippingAmount,
		Qty:               1,
		ProductTaxClassID: shippingTaxClassID,
	}

	// When shipping_includes_tax is set, the rate already contains tax — extract it.
	// In that case we record ShippingTaxAmount but do NOT add to TaxAmount (it's already
	// in ShippingAmount). The grand total formula then works without change.
	shippingIncludesTax := c.CP.GetBool("tax/calculation/shipping_includes_tax", cc.StoreID)

	taxResults, err := c.TaxRepo.CalculateTax(ctx, cc.Address.CountryID, regionID, postcode, []*repository.CartItemData{shippingItem}, customerTaxClassID, shippingIncludesTax)
	if err != nil {
		return nil // skip shipping tax on error
	}

	for _, tr := range taxResults {
		shippingTax := math.Round(tr.TaxAmount*100) / 100
		if !shippingIncludesTax {
			total.TaxAmount += shippingTax
		}
		total.ShippingTaxAmount += shippingTax
		total.AppliedTaxes = append(total.AppliedTaxes, AppliedTax{
			Label:  tr.Label + " (shipping)",
			Amount: shippingTax,
			Rate:   tr.Rate,
		})
	}

	return nil
}
