package totals

import (
	"context"

	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
)

// TaxCollector computes tax for cart items based on the shipping address.
// Sort order: 450. Runs after shipping so shipping amount is available.
//
// Tax calculation follows Magento's logic:
//  1. Batch-load product tax class IDs (one query for all items)
//  2. Find matching tax rate by country + region (from tax_calculation_rate)
//  3. Verify rate is linked to a rule matching product + customer tax class
//  4. Apply rate to taxable item row totals
type TaxCollector struct {
	TaxRepo *repository.TaxRepository
}

func (c *TaxCollector) Code() string { return "tax" }

func (c *TaxCollector) Collect(ctx context.Context, cc *CollectorContext, total *Total) error {
	if cc.Address == nil || cc.Address.CountryID == "" {
		return nil
	}
	if len(cc.Items) == 0 {
		return nil
	}

	// Batch-load product tax class IDs in ONE query
	productIDs := make([]int, 0, len(cc.Items))
	for _, item := range cc.Items {
		if item.ParentItemID == nil {
			productIDs = append(productIDs, item.ProductID)
		}
	}

	taxClasses, err := c.TaxRepo.GetProductTaxClassIDs(ctx, productIDs)
	if err != nil {
		return err
	}

	// Apply resolved tax classes to items
	for _, item := range cc.Items {
		if tc, ok := taxClasses[item.ProductID]; ok {
			item.ProductTaxClassID = tc
		}
	}

	// Resolve address fields
	regionID := 0
	if cc.Address.RegionID != nil {
		regionID = *cc.Address.RegionID
	}
	postcode := "*"
	if cc.Address.Postcode != nil {
		postcode = *cc.Address.Postcode
	}

	// Default customer tax class = 3 (Retail Customer)
	// TODO: resolve from customer_group when customer carts are supported
	customerTaxClassID := 3

	taxResults, err := c.TaxRepo.CalculateTax(ctx, cc.Address.CountryID, regionID, postcode, cc.Items, customerTaxClassID)
	if err != nil {
		return err
	}

	for _, tr := range taxResults {
		total.TaxAmount += tr.TaxAmount
		total.AppliedTaxes = append(total.AppliedTaxes, AppliedTax{
			Label:  tr.Label,
			Amount: tr.TaxAmount,
			Rate:   tr.Rate,
		})
	}

	return nil
}
