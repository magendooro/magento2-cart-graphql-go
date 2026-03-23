package repository

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// TaxRate holds a matched tax rate for an address.
type TaxRate struct {
	RateID    int
	Code      string
	Rate      float64 // percentage, e.g. 8.25
	CountryID string
	RegionID  int
	Postcode  string
}

// TaxResult holds computed tax for a cart.
type TaxResult struct {
	TaxAmount float64
	Label     string
	Rate      float64
}

type TaxRepository struct {
	db *sql.DB
}

func NewTaxRepository(db *sql.DB) *TaxRepository {
	return &TaxRepository{db: db}
}

// CalculateTax computes tax for the given items based on the shipping address.
//
// Tax calculation follows Magento's logic:
//  1. Find matching tax rate by country_id + region_id (from tax_calculation_rate)
//  2. Verify the rate is linked to a tax rule (via tax_calculation) that matches
//     the product's tax_class_id and customer's tax_class_id
//  3. Apply rate to taxable item subtotals
//
// Scope: US state-level tax only (region-based). No VAT/GST, no compound tax,
// no tax-inclusive pricing, no cross-border tax. EU tax support requires a
// separate implementation with country-level rates and VAT ID validation.
func (r *TaxRepository) CalculateTax(ctx context.Context, countryID string, regionID int, postcode string, items []*CartItemData, customerTaxClassID int) ([]*TaxResult, error) {
	if len(items) == 0 || countryID == "" {
		return nil, nil
	}

	// Find matching tax rates for this address
	rows, err := r.db.QueryContext(ctx, `
		SELECT tcr.tax_calculation_rate_id, tcr.code, tcr.rate,
		       tc.product_tax_class_id
		FROM tax_calculation_rate tcr
		JOIN tax_calculation tc ON tcr.tax_calculation_rate_id = tc.tax_calculation_rate_id
		JOIN tax_calculation_rule tcrl ON tc.tax_calculation_rule_id = tcrl.tax_calculation_rule_id
		WHERE tcr.tax_country_id = ?
		  AND (tcr.tax_region_id = ? OR tcr.tax_region_id = 0)
		  AND (tcr.tax_postcode = ? OR tcr.tax_postcode = '*')
		  AND tc.customer_tax_class_id = ?
		ORDER BY tcrl.priority ASC, tcr.tax_region_id DESC`,
		countryID, regionID, postcode, customerTaxClassID,
	)
	if err != nil {
		return nil, fmt.Errorf("tax rate lookup: %w", err)
	}
	defer rows.Close()

	// Build rate map: product_tax_class_id → (rate, label)
	type rateInfo struct {
		rate  float64
		label string
	}
	ratesByProductClass := make(map[int]*rateInfo)

	for rows.Next() {
		var rateID int
		var code string
		var rate float64
		var productTaxClassID int
		if err := rows.Scan(&rateID, &code, &rate, &productTaxClassID); err != nil {
			continue
		}
		if _, exists := ratesByProductClass[productTaxClassID]; !exists {
			ratesByProductClass[productTaxClassID] = &rateInfo{rate: rate, label: code}
		}
	}

	if len(ratesByProductClass) == 0 {
		return nil, nil
	}

	// Calculate tax per item based on product tax class
	taxByLabel := make(map[string]float64)

	for _, item := range items {
		if item.ParentItemID != nil {
			continue // skip child items
		}
		ri, ok := ratesByProductClass[item.ProductTaxClassID]
		if !ok || ri.rate == 0 {
			continue // product not taxable under this rule
		}
		itemTax := item.RowTotal * ri.rate / 100.0
		taxByLabel[ri.label] += itemTax
	}

	var results []*TaxResult
	for label, amount := range taxByLabel {
		ri := ratesByProductClass[0] // get any rate for the percentage
		for _, r := range ratesByProductClass {
			ri = r
			break
		}
		results = append(results, &TaxResult{
			TaxAmount: math.Round(amount*100) / 100,
			Label:     label,
			Rate:      ri.rate,
		})
	}

	return results, nil
}

// GetProductTaxClassID returns the tax_class_id for a product.
// Falls back to eav_attribute.default_value when no explicit value is stored.
func (r *TaxRepository) GetProductTaxClassID(ctx context.Context, productID int) int {
	var taxClassID int
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(cpei.value, ea.default_value, 0)
		FROM eav_attribute ea
		LEFT JOIN catalog_product_entity_int cpei
			ON cpei.attribute_id = ea.attribute_id
			AND cpei.entity_id = ? AND cpei.store_id = 0
		WHERE ea.attribute_code = 'tax_class_id' AND ea.entity_type_id = 4`,
		productID,
	).Scan(&taxClassID)
	if err != nil {
		return 0
	}
	return taxClassID
}
