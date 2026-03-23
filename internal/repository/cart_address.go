package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CartAddressData holds a quote_address row.
type CartAddressData struct {
	AddressID          int
	QuoteID            int
	AddressType        string // "shipping" or "billing"
	Firstname          string
	Lastname           string
	Company            *string
	Street             string // newline-separated
	City               string
	Region             *string
	RegionID           *int
	Postcode           *string
	CountryID          string
	Telephone          *string
	ShippingMethod     *string
	ShippingDescription *string
	ShippingAmount     float64
	SubtotalInclTax    float64
	TaxAmount          float64
	GrandTotal         float64
}

type CartAddressRepository struct {
	db *sql.DB
}

func NewCartAddressRepository(db *sql.DB) *CartAddressRepository {
	return &CartAddressRepository{db: db}
}

// GetByQuoteID loads all addresses for a cart.
func (r *CartAddressRepository) GetByQuoteID(ctx context.Context, quoteID int) ([]*CartAddressData, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT address_id, quote_id, address_type, COALESCE(firstname,''), COALESCE(lastname,''),
		       company, COALESCE(street,''), COALESCE(city,''), region, region_id,
		       postcode, COALESCE(country_id,''), telephone,
		       shipping_method, shipping_description,
		       COALESCE(shipping_amount, 0), COALESCE(subtotal_incl_tax, 0),
		       COALESCE(tax_amount, 0), COALESCE(grand_total, 0)
		FROM quote_address WHERE quote_id = ? ORDER BY address_type`, quoteID)
	if err != nil {
		return nil, fmt.Errorf("load cart addresses: %w", err)
	}
	defer rows.Close()

	var addrs []*CartAddressData
	for rows.Next() {
		var a CartAddressData
		if err := rows.Scan(
			&a.AddressID, &a.QuoteID, &a.AddressType, &a.Firstname, &a.Lastname,
			&a.Company, &a.Street, &a.City, &a.Region, &a.RegionID,
			&a.Postcode, &a.CountryID, &a.Telephone,
			&a.ShippingMethod, &a.ShippingDescription,
			&a.ShippingAmount, &a.SubtotalInclTax, &a.TaxAmount, &a.GrandTotal,
		); err != nil {
			return nil, fmt.Errorf("scan address: %w", err)
		}
		addrs = append(addrs, &a)
	}
	return addrs, rows.Err()
}

// SetAddress inserts or updates an address on the cart.
func (r *CartAddressRepository) SetAddress(ctx context.Context, quoteID int, addressType string, firstname, lastname, city, countryID string, street []string, company, region, postcode, telephone *string, regionID *int) (int, error) {
	streetStr := strings.Join(street, "\n")

	// Check if address of this type already exists
	var existingID int
	err := r.db.QueryRowContext(ctx,
		"SELECT address_id FROM quote_address WHERE quote_id = ? AND address_type = ?",
		quoteID, addressType,
	).Scan(&existingID)

	if err == nil {
		// Update existing
		_, err := r.db.ExecContext(ctx, `
			UPDATE quote_address SET firstname = ?, lastname = ?, company = ?,
				street = ?, city = ?, region = ?, region_id = ?, postcode = ?,
				country_id = ?, telephone = ?, updated_at = NOW()
			WHERE address_id = ?`,
			firstname, lastname, company, streetStr, city,
			region, regionID, postcode, countryID, telephone, existingID,
		)
		if err != nil {
			return 0, fmt.Errorf("update address: %w", err)
		}
		return existingID, nil
	}

	// Insert new
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO quote_address (quote_id, address_type, firstname, lastname, company,
			street, city, region, region_id, postcode, country_id, telephone,
			created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW())`,
		quoteID, addressType, firstname, lastname, company,
		streetStr, city, region, regionID, postcode, countryID, telephone,
	)
	if err != nil {
		return 0, fmt.Errorf("insert address: %w", err)
	}
	id, _ := result.LastInsertId()
	return int(id), nil
}

// ResolveRegion looks up region name and code from directory_country_region.
func (r *CartAddressRepository) ResolveRegion(ctx context.Context, regionID int) (string, string, error) {
	var code, name string
	err := r.db.QueryRowContext(ctx,
		"SELECT code, default_name FROM directory_country_region WHERE region_id = ?", regionID,
	).Scan(&code, &name)
	return code, name, err
}
