package repository

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
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
// When priceIncludesTax is true (tax/calculation/price_includes_tax = 1),
// tax is extracted from the row total using:
//
//	tax = rowTotal * rate / (100 + rate)
//
// Otherwise the standard exclusive formula is used: tax = rowTotal * rate / 100.
func (r *TaxRepository) CalculateTax(ctx context.Context, countryID string, regionID int, postcode string, items []*CartItemData, customerTaxClassID int, priceIncludesTax bool) ([]*TaxResult, error) {
	if len(items) == 0 || countryID == "" {
		return nil, nil
	}

	// Find matching tax rates for this address, grouped by priority
	rows, err := r.db.QueryContext(ctx, `
		SELECT tcr.tax_calculation_rate_id, tcr.code, tcr.rate,
		       tc.product_tax_class_id, tcrl.priority
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

	// Collect all matching rates with their priority and product class
	type rateEntry struct {
		code            string
		rate            float64
		productClassID  int
		priority        int
	}
	var allRates []rateEntry
	seen := make(map[string]bool) // deduplicate by code+productClass

	for rows.Next() {
		var rateID, productTaxClassID, priority int
		var code string
		var rate float64
		if err := rows.Scan(&rateID, &code, &rate, &productTaxClassID, &priority); err != nil {
			continue
		}
		key := fmt.Sprintf("%s-%d", code, productTaxClassID)
		if !seen[key] {
			seen[key] = true
			allRates = append(allRates, rateEntry{code: code, rate: rate, productClassID: productTaxClassID, priority: priority})
		}
	}

	if len(allRates) == 0 {
		return nil, nil
	}

	// Group rates by priority for compound calculation
	// Same priority = additive (rates sum on original amount)
	// Different priority = compound (apply on already-taxed amount)
	type priorityGroup struct {
		priority int
		rates    []rateEntry
	}
	var groups []priorityGroup
	lastPriority := -1
	for _, r := range allRates {
		if r.priority != lastPriority {
			groups = append(groups, priorityGroup{priority: r.priority})
			lastPriority = r.priority
		}
		groups[len(groups)-1].rates = append(groups[len(groups)-1].rates, r)
	}

	// Calculate tax per item with compound support
	taxByLabel := make(map[string]float64)

	for _, item := range items {
		if item.ParentItemID != nil {
			continue
		}
		taxableAmount := item.RowTotal

		for _, group := range groups {
			groupTax := 0.0
			for _, r := range group.rates {
				if r.productClassID != item.ProductTaxClassID {
					continue
				}
				var rateTax float64
			if priceIncludesTax {
				// Extraction: tax already embedded in price
				rateTax = taxableAmount * r.rate / (100.0 + r.rate)
			} else {
				rateTax = taxableAmount * r.rate / 100.0
			}
				groupTax += rateTax
				taxByLabel[r.code] += rateTax
			}
			// For compound: next priority group taxes the amount + previous tax
			taxableAmount += groupTax
		}
	}

	var results []*TaxResult
	for _, r := range allRates {
		amount, ok := taxByLabel[r.code]
		if !ok || amount == 0 {
			continue
		}
		results = append(results, &TaxResult{
			TaxAmount: math.Round(amount*100) / 100,
			Label:     r.code,
			Rate:      r.rate,
		})
		delete(taxByLabel, r.code) // prevent duplicates
	}

	return results, nil
}

// GetProductTaxClassID returns the tax_class_id for a single product.
// Falls back to eav_attribute.default_value when no explicit value is stored.
func (r *TaxRepository) GetProductTaxClassID(ctx context.Context, productID int) int {
	result, err := r.GetProductTaxClassIDs(ctx, []int{productID})
	if err != nil {
		return 0
	}
	return result[productID]
}

// GetProductTaxClassIDs batch-loads tax_class_id for multiple products in a single query.
// Falls back to eav_attribute.default_value when no explicit value is stored.
// Returns map[productID]taxClassID.
func (r *TaxRepository) GetProductTaxClassIDs(ctx context.Context, productIDs []int) (map[int]int, error) {
	result := make(map[int]int, len(productIDs))
	if len(productIDs) == 0 {
		return result, nil
	}

	// Build IN clause
	placeholders := make([]string, len(productIDs))
	args := make([]interface{}, len(productIDs))
	for i, id := range productIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `
		SELECT cpei.entity_id, COALESCE(cpei.value, ea.default_value, 0)
		FROM eav_attribute ea
		JOIN catalog_product_entity_int cpei
			ON cpei.attribute_id = ea.attribute_id
			AND cpei.store_id = 0
			AND cpei.entity_id IN (` + strings.Join(placeholders, ",") + `)
		WHERE ea.attribute_code = 'tax_class_id' AND ea.entity_type_id = 4`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return result, fmt.Errorf("batch tax class lookup: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var productID, taxClassID int
		if err := rows.Scan(&productID, &taxClassID); err != nil {
			continue
		}
		result[productID] = taxClassID
	}

	// For products not found (no EAV row), use attribute default_value
	if len(result) < len(productIDs) {
		var defaultValue int
		r.db.QueryRowContext(ctx,
			"SELECT COALESCE(default_value, 0) FROM eav_attribute WHERE attribute_code = 'tax_class_id' AND entity_type_id = 4",
		).Scan(&defaultValue)
		for _, id := range productIDs {
			if _, ok := result[id]; !ok {
				result[id] = defaultValue
			}
		}
	}

	return result, rows.Err()
}

// GetCustomerTaxClassID returns the tax_class_id for the given customer_group_id.
// Returns 3 (Retail Customer) as the fallback for any error or missing row.
func (r *TaxRepository) GetCustomerTaxClassID(ctx context.Context, customerGroupID int) int {
	var classID int
	err := r.db.QueryRowContext(ctx,
		"SELECT COALESCE(tax_class_id, 3) FROM customer_group WHERE customer_group_id = ?",
		customerGroupID,
	).Scan(&classID)
	if err != nil {
		return 3 // Retail Customer fallback
	}
	return classID
}
