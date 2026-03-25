package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/magendooro/magento2-go-common/config"

	"github.com/magendooro/magento2-cart-graphql-go/internal/shipping"
)

// ShippingRate holds a single available shipping rate.
type ShippingRate struct {
	CarrierCode  string
	CarrierTitle string
	MethodCode   string
	MethodTitle  string
	Price        float64
}

type ShippingRepository struct {
	db *sql.DB
	cp *config.ConfigProvider
}

func NewShippingRepository(db *sql.DB, cp *config.ConfigProvider) *ShippingRepository {
	return &ShippingRepository{db: db, cp: cp}
}

// GetAvailableRates returns shipping rates for the given address and cart subtotal.
// itemQty is needed for per-item flatrate calculation.
func (r *ShippingRepository) GetAvailableRates(ctx context.Context, storeID int, countryID string, regionID *int, postcode *string, subtotal float64, itemQty float64) ([]*ShippingRate, error) {
	var rates []*ShippingRate

	// Tablerate carrier
	if r.cp.GetBool("carriers/tablerate/active", storeID) {
		tr, err := r.getTablerateRates(ctx, storeID, countryID, regionID, postcode, subtotal)
		if err == nil && tr != nil {
			rates = append(rates, tr)
		}
	}

	// Flatrate carrier (active by default in Magento when not explicitly configured)
	if r.cp.GetInt("carriers/flatrate/active", storeID, 1) == 1 {
		unitPrice := r.cp.GetFloat("carriers/flatrate/price", storeID, 5.0)
		// Magento default type is "I" (per-item). "O" = per-order.
		flatrateType := r.cp.Get("carriers/flatrate/type", storeID)
		if flatrateType == "" {
			flatrateType = "I" // default: per-item
		}
		price := unitPrice
		if flatrateType == "I" && itemQty > 0 {
			price = unitPrice * itemQty
		}
		title := r.cp.Get("carriers/flatrate/title", storeID)
		if title == "" {
			title = "Flat Rate"
		}
		methodTitle := r.cp.Get("carriers/flatrate/name", storeID)
		if methodTitle == "" {
			methodTitle = "Fixed"
		}
		rates = append(rates, &ShippingRate{
			CarrierCode:  "flatrate",
			CarrierTitle: title,
			MethodCode:   "flatrate",
			MethodTitle:  methodTitle,
			Price:        price,
		})
	}

	// Freeshipping carrier
	if r.cp.GetBool("carriers/freeshipping/active", storeID) {
		threshold := r.cp.GetFloat("carriers/freeshipping/free_shipping_subtotal", storeID, 0)
		if threshold == 0 || subtotal >= threshold {
			rates = append(rates, &ShippingRate{
				CarrierCode:  "freeshipping",
				CarrierTitle: "Free Shipping",
				MethodCode:   "freeshipping",
				MethodTitle:  "Free",
				Price:        0,
			})
		}
	}

	return rates, nil
}

func (r *ShippingRepository) getTablerateRates(ctx context.Context, storeID int, countryID string, regionID *int, postcode *string, subtotal float64) (*ShippingRate, error) {
	// Tablerate lookup: find the best matching rate
	// Magento matches by (website, country, region, postcode, condition_value) with fallback
	websiteID := r.cp.GetWebsiteID(storeID)

	var price float64
	rid := 0
	if regionID != nil {
		rid = *regionID
	}
	pc := "*"
	if postcode != nil && *postcode != "" {
		pc = *postcode
	}

	// Try exact match first, then fallback to wildcard
	queries := []string{
		"SELECT price FROM shipping_tablerate WHERE website_id = ? AND dest_country_id = ? AND dest_region_id = ? AND dest_zip = ? AND condition_value <= ? ORDER BY condition_value DESC LIMIT 1",
		"SELECT price FROM shipping_tablerate WHERE website_id = ? AND dest_country_id = ? AND dest_region_id = ? AND dest_zip = '*' AND condition_value <= ? ORDER BY condition_value DESC LIMIT 1",
		"SELECT price FROM shipping_tablerate WHERE website_id = ? AND dest_country_id = ? AND dest_region_id = 0 AND dest_zip = '*' AND condition_value <= ? ORDER BY condition_value DESC LIMIT 1",
		"SELECT price FROM shipping_tablerate WHERE website_id = ? AND dest_country_id = '0' AND dest_region_id = 0 AND dest_zip = '*' AND condition_value <= ? ORDER BY condition_value DESC LIMIT 1",
	}

	found := false
	for i, q := range queries {
		var err error
		switch i {
		case 0:
			err = r.db.QueryRowContext(ctx, q, websiteID, countryID, rid, pc, subtotal).Scan(&price)
		case 1:
			err = r.db.QueryRowContext(ctx, q, websiteID, countryID, rid, subtotal).Scan(&price)
		case 2:
			err = r.db.QueryRowContext(ctx, q, websiteID, countryID, subtotal).Scan(&price)
		case 3:
			err = r.db.QueryRowContext(ctx, q, websiteID, subtotal).Scan(&price)
		}
		if err == nil {
			found = true
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("no tablerate found")
	}

	title := r.cp.Get("carriers/tablerate/title", 0)
	if title == "" {
		title = "Best Way"
	}
	methodTitle := r.cp.Get("carriers/tablerate/name", 0)
	if methodTitle == "" {
		methodTitle = "Table Rate"
	}

	return &ShippingRate{
		CarrierCode:  "tablerate",
		CarrierTitle: title,
		MethodCode:   "bestway",
		MethodTitle:  methodTitle,
		Price:        price,
	}, nil
}

// SetShippingMethod sets the selected shipping method on a quote_address.
func (r *ShippingRepository) SetShippingMethod(ctx context.Context, addressID int, carrierCode, methodCode string, amount float64, description string) error {
	method := carrierCode + "_" + methodCode
	_, err := r.db.ExecContext(ctx, `
		UPDATE quote_address SET shipping_method = ?, shipping_description = ?,
			shipping_amount = ?, base_shipping_amount = ?, updated_at = NOW()
		WHERE address_id = ?`,
		method, description, amount, amount, addressID,
	)
	return err
}

// StoredShippingRate mirrors a quote_shipping_rate row.
type StoredShippingRate struct {
	Code         string // carrier_code + "_" + method_code
	Carrier      string
	CarrierTitle string
	Method       string
	MethodTitle  string
	Price        float64
}

// SaveRates replaces all stored rates for an address in quote_shipping_rate.
// Magento writes rates here when collectShippingRates() runs; we mirror that
// so selected_shipping_method can read carrier_title/method_title from storage
// rather than re-deriving them live.
func (r *ShippingRepository) SaveRates(ctx context.Context, addressID int, rates []*shipping.Rate) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM quote_shipping_rate WHERE address_id = ?", addressID)
	if err != nil {
		return err
	}
	for _, rate := range rates {
		code := rate.CarrierCode + "_" + rate.MethodCode
		_, err := r.db.ExecContext(ctx, `
			INSERT INTO quote_shipping_rate
			    (address_id, carrier, carrier_title, code, method, method_title, price, error_message)
			VALUES (?, ?, ?, ?, ?, ?, ?, '')`,
			addressID, rate.CarrierCode, rate.CarrierTitle,
			code, rate.MethodCode, rate.MethodTitle, rate.Price,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// LoadRateByCode returns the stored rate for an address whose code matches
// the given shipping_method string (e.g. "tablerate_bestway"). Returns nil
// if no matching rate is found.
func (r *ShippingRepository) LoadRateByCode(ctx context.Context, addressID int, code string) *StoredShippingRate {
	var sr StoredShippingRate
	err := r.db.QueryRowContext(ctx, `
		SELECT code, carrier, carrier_title, method, method_title, price
		FROM quote_shipping_rate
		WHERE address_id = ? AND code = ?`,
		addressID, code,
	).Scan(&sr.Code, &sr.Carrier, &sr.CarrierTitle, &sr.Method, &sr.MethodTitle, &sr.Price)
	if err != nil {
		return nil
	}
	return &sr
}
