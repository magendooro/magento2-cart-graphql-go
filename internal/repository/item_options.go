package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
)

// WriteItemOption inserts a single row into quote_item_option.
func (r *CartItemRepository) WriteItemOption(ctx context.Context, itemID, productID int, code, value string) error {
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO quote_item_option (item_id, product_id, code, value) VALUES (?, ?, ?, ?)",
		itemID, productID, code, value,
	)
	return err
}

// GetItemOptions returns all quote_item_option code→value pairs for an item.
func (r *CartItemRepository) GetItemOptions(ctx context.Context, itemID int) map[string]string {
	rows, err := r.db.QueryContext(ctx,
		"SELECT code, value FROM quote_item_option WHERE item_id = ?", itemID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	opts := make(map[string]string)
	for rows.Next() {
		var code, value string
		rows.Scan(&code, &value)
		opts[code] = value
	}
	return opts
}

// BuildProductOptionsJSON constructs the product_options JSON blob for a sales_order_item row.
//
// For simple products: {"info_buyRequest":{"qty":N,"options":[]}}
// For configurable parent items: full JSON including attributes_info, simple_name, simple_sku.
// For child items (parent_item_id != nil): nil (handled by returning empty string).
func BuildProductOptionsJSON(ctx context.Context, db *sql.DB, item *CartItemData, allItems []*CartItemData) string {
	// Child items of configurable/bundle do not carry product_options in Magento
	if item.ParentItemID != nil {
		return ""
	}

	switch item.ProductType {
	case "configurable":
		return buildConfigurableOptions(ctx, db, item, allItems)
	default:
		// Simple, virtual, downloadable
		return buildSimpleOptions(item)
	}
}

func buildSimpleOptions(item *CartItemData) string {
	type buyRequest struct {
		Qty     float64       `json:"qty"`
		Options []interface{} `json:"options"`
	}
	type opts struct {
		InfoBuyRequest buyRequest `json:"info_buyRequest"`
	}
	b, _ := json.Marshal(opts{
		InfoBuyRequest: buyRequest{Qty: item.Qty, Options: []interface{}{}},
	})
	return string(b)
}

func buildConfigurableOptions(ctx context.Context, db *sql.DB, item *CartItemData, allItems []*CartItemData) string {
	// Read stored options from quote_item_option
	rows, err := db.QueryContext(ctx,
		"SELECT code, value FROM quote_item_option WHERE item_id = ?", item.ItemID)
	if err != nil {
		return buildSimpleOptions(item)
	}
	defer rows.Close()
	opts := make(map[string]string)
	for rows.Next() {
		var code, value string
		rows.Scan(&code, &value)
		opts[code] = value
	}

	// Parse the attributes JSON: {"<attr_id>": "<option_id>", ...}
	var rawAttrs map[string]string
	if v := opts["attributes"]; v != "" {
		json.Unmarshal([]byte(v), &rawAttrs)
	}

	// Find the child item for simple_name / simple_sku
	simpleName, simpleSKU := "", ""
	for _, other := range allItems {
		if other.ParentItemID != nil && *other.ParentItemID == item.ItemID {
			simpleName = other.Name
			simpleSKU = other.SKU
			break
		}
	}

	// Build attributes_info by looking up EAV label and option value text
	type attrInfo struct {
		Label       string `json:"label"`
		Value       string `json:"value"`
		OptionID    int    `json:"option_id"`
		OptionValue string `json:"option_value"`
	}
	var attributesInfo []attrInfo
	for attrIDStr, optIDStr := range rawAttrs {
		attrID, _ := strconv.Atoi(attrIDStr)
		optID, _ := strconv.Atoi(optIDStr)
		if attrID == 0 {
			continue
		}

		var label string
		db.QueryRowContext(ctx,
			"SELECT COALESCE(frontend_label, attribute_code) FROM eav_attribute WHERE attribute_id = ?",
			attrID,
		).Scan(&label)

		var optValue string
		db.QueryRowContext(ctx, `
			SELECT COALESCE(sov.value, gov.value, ?)
			FROM eav_attribute_option eao
			LEFT JOIN eav_attribute_option_value sov ON sov.option_id = eao.option_id AND sov.store_id = 1
			LEFT JOIN eav_attribute_option_value gov ON gov.option_id = eao.option_id AND gov.store_id = 0
			WHERE eao.option_id = ?`,
			optIDStr, optID,
		).Scan(&optValue)

		attributesInfo = append(attributesInfo, attrInfo{
			Label:       label,
			Value:       optValue,
			OptionID:    attrID,
			OptionValue: optIDStr,
		})
	}

	// Build info_buyRequest.super_attribute
	superAttr := make(map[string]string)
	for k, v := range rawAttrs {
		superAttr[k] = v
	}

	type buyRequest struct {
		Qty            float64           `json:"qty"`
		SuperAttribute map[string]string `json:"super_attribute"`
		Options        []interface{}     `json:"options"`
	}
	type productOpts struct {
		InfoBuyRequest    buyRequest `json:"info_buyRequest"`
		AttributesInfo    []attrInfo `json:"attributes_info"`
		SimpleName        string     `json:"simple_name"`
		SimpleSKU         string     `json:"simple_sku"`
		ProductCalc       int        `json:"product_calculations"`
		ShipmentType      int        `json:"shipment_type"`
	}

	if attributesInfo == nil {
		attributesInfo = []attrInfo{}
	}

	p := productOpts{
		InfoBuyRequest: buyRequest{
			Qty:            item.Qty,
			SuperAttribute: superAttr,
			Options:        []interface{}{},
		},
		AttributesInfo:  attributesInfo,
		SimpleName:      simpleName,
		SimpleSKU:       simpleSKU,
		ProductCalc:     1,
		ShipmentType:    0,
	}

	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Sprintf(`{"info_buyRequest":{"qty":%v,"options":[]}}`, item.Qty)
	}
	return string(b)
}
