package shipping

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/magendooro/magento2-go-common/config"
)

// TablerateCarrier implements table rate shipping.
// Looks up rates from shipping_tablerate by (website, country, region, postcode,
// condition_name, condition_value) with multi-level address fallback.
type TablerateCarrier struct {
	DB *sql.DB
	CP *config.ConfigProvider
}

func (c *TablerateCarrier) Code() string { return "tablerate" }

func (c *TablerateCarrier) IsActive(storeID int) bool {
	return c.CP.GetBool("carriers/tablerate/active", storeID)
}

func (c *TablerateCarrier) CollectRates(ctx context.Context, req *RateRequest) ([]*Rate, error) {
	conditionName := c.CP.Get("carriers/tablerate/condition_name", req.StoreID)
	if conditionName == "" {
		conditionName = "package_value"
	}

	conditionValue := conditionValueFor(conditionName, req)

	rid := 0
	if req.RegionID != nil {
		rid = *req.RegionID
	}
	pc := "*"
	if req.Postcode != nil && *req.Postcode != "" {
		pc = *req.Postcode
	}

	price, err := c.lookupRate(ctx, req.WebsiteID, req.CountryID, rid, pc, conditionName, conditionValue)
	if err != nil {
		return nil, fmt.Errorf("no tablerate found")
	}

	title := c.CP.Get("carriers/tablerate/title", 0)
	if title == "" {
		title = "Best Way"
	}
	methodTitle := c.CP.Get("carriers/tablerate/name", 0)
	if methodTitle == "" {
		methodTitle = "Table Rate"
	}

	return []*Rate{{
		CarrierCode:  "tablerate",
		CarrierTitle: title,
		MethodCode:   "bestway",
		MethodTitle:  methodTitle,
		Price:        price,
	}}, nil
}

// conditionValueFor maps a condition_name to the corresponding value from the request.
func conditionValueFor(conditionName string, req *RateRequest) float64 {
	switch conditionName {
	case "package_qty":
		return req.ItemQty
	case "package_weight":
		return req.Weight
	case "package_value_with_discount":
		return req.SubtotalWithDiscount
	default: // package_value
		return req.Subtotal
	}
}

// lookupRate tries exact match then progressively more general address fallbacks.
func (c *TablerateCarrier) lookupRate(ctx context.Context, websiteID int, countryID string, regionID int, postcode string, conditionName string, conditionValue float64) (float64, error) {
	queries := []struct {
		sql  string
		args []interface{}
	}{
		{
			"SELECT price FROM shipping_tablerate WHERE website_id = ? AND dest_country_id = ? AND dest_region_id = ? AND dest_zip = ? AND condition_name = ? AND condition_value <= ? ORDER BY condition_value DESC LIMIT 1",
			[]interface{}{websiteID, countryID, regionID, postcode, conditionName, conditionValue},
		},
		{
			"SELECT price FROM shipping_tablerate WHERE website_id = ? AND dest_country_id = ? AND dest_region_id = ? AND dest_zip = '*' AND condition_name = ? AND condition_value <= ? ORDER BY condition_value DESC LIMIT 1",
			[]interface{}{websiteID, countryID, regionID, conditionName, conditionValue},
		},
		{
			"SELECT price FROM shipping_tablerate WHERE website_id = ? AND dest_country_id = ? AND dest_region_id = 0 AND dest_zip = '*' AND condition_name = ? AND condition_value <= ? ORDER BY condition_value DESC LIMIT 1",
			[]interface{}{websiteID, countryID, conditionName, conditionValue},
		},
		{
			"SELECT price FROM shipping_tablerate WHERE website_id = ? AND dest_country_id = '0' AND dest_region_id = 0 AND dest_zip = '*' AND condition_name = ? AND condition_value <= ? ORDER BY condition_value DESC LIMIT 1",
			[]interface{}{websiteID, conditionName, conditionValue},
		},
	}

	for _, q := range queries {
		var price float64
		if err := c.DB.QueryRowContext(ctx, q.sql, q.args...).Scan(&price); err == nil {
			return price, nil
		}
	}
	return 0, fmt.Errorf("no matching tablerate")
}
