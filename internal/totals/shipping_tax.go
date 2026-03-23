package totals

import (
	"context"
	"math"

	"github.com/magendooro/magento2-cart-graphql-go/internal/config"
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

	// Default customer tax class = 3 (Retail Customer)
	customerTaxClassID := 3

	// Create a virtual item representing the shipping amount with the shipping tax class
	shippingItem := &repository.CartItemData{
		ItemID:            -1, // virtual item
		RowTotal:          total.ShippingAmount,
		Qty:               1,
		ProductTaxClassID: shippingTaxClassID,
	}

	taxResults, err := c.TaxRepo.CalculateTax(ctx, cc.Address.CountryID, regionID, postcode, []*repository.CartItemData{shippingItem}, customerTaxClassID)
	if err != nil {
		return nil // skip shipping tax on error
	}

	for _, tr := range taxResults {
		shippingTax := math.Round(tr.TaxAmount*100) / 100
		total.TaxAmount += shippingTax
		total.AppliedTaxes = append(total.AppliedTaxes, AppliedTax{
			Label:  tr.Label + " (shipping)",
			Amount: shippingTax,
			Rate:   tr.Rate,
		})
	}

	return nil
}
