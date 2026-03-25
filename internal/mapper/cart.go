package mapper

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/magendooro/magento2-cart-graphql-go/graph/model"
	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
	"github.com/magendooro/magento2-cart-graphql-go/internal/shipping"
	"github.com/magendooro/magento2-cart-graphql-go/internal/totals"
)

// CartMapper converts repository data into GraphQL model types.
// It holds only the dependencies needed for presentation-layer lookups
// (inline configurable/bundle option queries and region resolution).
type CartMapper struct {
	db          *sql.DB
	addressRepo *repository.CartAddressRepository
}

func NewCartMapper(db *sql.DB, addressRepo *repository.CartAddressRepository) *CartMapper {
	return &CartMapper{db: db, addressRepo: addressRepo}
}

// MapCartInput bundles all pre-fetched data the mapper needs to produce a Cart model.
// The service is responsible for fetching all of these before calling MapCart.
type MapCartInput struct {
	Cart            *repository.CartData
	Items           []*repository.CartItemData
	Addrs           []*repository.CartAddressData
	DisplayTotals   *totals.Total
	ShippingRates   map[int][]*shipping.Rate // keyed by address ID
	AvailPayments   []*repository.PaymentMethod
	SelectedPayment *repository.PaymentMethod // nil if none selected
	MaskedID        string
	MediaBaseURL    string // e.g. "http://localhost/media/catalog/product"
}

// MapCart produces the full GraphQL Cart object from pre-fetched service data.
func (m *CartMapper) MapCart(ctx context.Context, in MapCartInput) *model.Cart {
	currency := model.CurrencyEnum(in.Cart.QuoteCurrencyCode)

	// Compute product tax and tax mode once, reused for both items and subtotals.
	var productTaxAmount float64
	taxIncludedInPrice := in.DisplayTotals != nil && in.DisplayTotals.TaxIncludedInPrice
	if in.DisplayTotals != nil {
		productTaxAmount = in.DisplayTotals.TaxAmount - in.DisplayTotals.ShippingTaxAmount
	}

	// Batch-load thumbnails for all product IDs present in the cart.
	var productIDs []int
	seen := make(map[int]bool)
	for _, item := range in.Items {
		if !seen[item.ProductID] {
			productIDs = append(productIDs, item.ProductID)
			seen[item.ProductID] = true
		}
	}
	thumbPaths := m.loadThumbnails(ctx, productIDs)
	thumbs := make(map[int]*model.CartItemProductImage, len(thumbPaths))
	for productID, path := range thumbPaths {
		url := in.MediaBaseURL + path
		thumbs[productID] = &model.CartItemProductImage{URL: &url}
	}

	urlKeys := m.loadURLKeys(ctx, productIDs)

	// Collect item IDs for custom option loading (only top-level items).
	var itemIDs []int
	for _, item := range in.Items {
		if item.ParentItemID == nil {
			itemIDs = append(itemIDs, item.ItemID)
		}
	}
	customOpts := m.loadCustomizableOptions(ctx, itemIDs)

	cartItems := make([]model.CartItemInterface, 0, len(in.Items))
	for _, item := range in.Items {
		if item.ParentItemID != nil {
			continue
		}
		rowTotalInclTax := itemRowTotalInclTax(item.ItemID, item.RowTotal, taxIncludedInPrice, in.DisplayTotals)
		cartItems = append(cartItems, m.MapCartItem(ctx, item, in.Items, currency, rowTotalInclTax, thumbs, urlKeys, customOpts))
	}

	// Applied taxes
	var appliedTaxes []*model.CartTaxItem
	if in.DisplayTotals != nil {
		for _, at := range in.DisplayTotals.AppliedTaxes {
			amt := at.Amount
			appliedTaxes = append(appliedTaxes, &model.CartTaxItem{
				Amount: &model.Money{Value: &amt, Currency: &currency},
				Label:  at.Label,
			})
		}
	}

	// Subtotal incl/excl tax
	var subtotalInclTax float64
	if taxIncludedInPrice {
		subtotalInclTax = in.Cart.Subtotal
	} else {
		subtotalInclTax = in.Cart.Subtotal + productTaxAmount
	}

	// Discount
	var discountAmount float64
	if in.DisplayTotals != nil {
		discountAmount = in.DisplayTotals.DiscountAmount
	}
	subtotalWithDiscount := math.Round((in.Cart.Subtotal-discountAmount)*100) / 100

	var discounts []*model.Discount
	if discountAmount > 0 {
		label := "Discount"
		if in.Cart.CouponCode != nil && *in.Cart.CouponCode != "" {
			label = *in.Cart.CouponCode
		}
		wholeCart := model.DiscountAppliedToTypeWholeCart
		discounts = append(discounts, &model.Discount{
			Amount:    &model.Money{Value: &discountAmount, Currency: &currency},
			Label:     label,
			AppliedTo: &wholeCart,
		})
	}

	result := &model.Cart{
		ID:            in.MaskedID,
		Items:         cartItems,
		TotalQuantity: in.Cart.ItemsQty,
		IsVirtual:     in.Cart.IsVirtual == 1,
		Email:         in.Cart.CustomerEmail,
		Prices: &model.CartPrices{
			GrandTotal:                      &model.Money{Value: &in.Cart.GrandTotal, Currency: &currency},
			SubtotalExcludingTax:            subtotalExclTax(in.Cart.Subtotal, in.DisplayTotals),
			SubtotalIncludingTax:            &model.Money{Value: &subtotalInclTax, Currency: &currency},
			SubtotalWithDiscountExcludingTax: &model.Money{Value: &subtotalWithDiscount, Currency: &currency},
			AppliedTaxes:                    appliedTaxes,
			Discounts:                       discounts,
		},
		ShippingAddresses: []*model.ShippingCartAddress{},
	}

	// Map addresses
	for _, a := range in.Addrs {
		switch a.AddressType {
		case "shipping":
			rates := in.ShippingRates[a.AddressID]
			sa := m.MapShippingAddress(ctx, a, rates, currency)
			result.ShippingAddresses = append(result.ShippingAddresses, sa)
		case "billing":
			result.BillingAddress = m.MapBillingAddress(ctx, a)
		}
	}

	// Coupon
	if in.Cart.CouponCode != nil && *in.Cart.CouponCode != "" {
		result.AppliedCoupons = []*model.AppliedCoupon{{Code: *in.Cart.CouponCode}}
	}

	// Payment methods
	for _, pm := range in.AvailPayments {
		result.AvailablePaymentMethods = append(result.AvailablePaymentMethods, &model.AvailablePaymentMethod{
			Code: pm.Code, Title: pm.Title,
		})
	}
	if in.SelectedPayment != nil && in.SelectedPayment.Code != "" {
		result.SelectedPaymentMethod = &model.SelectedPaymentMethod{
			Code: in.SelectedPayment.Code, Title: &in.SelectedPayment.Title,
		}
	}

	return result
}

// MapShippingAddress converts a shipping CartAddressData plus pre-fetched rates into the GraphQL type.
func (m *CartMapper) MapShippingAddress(ctx context.Context, a *repository.CartAddressData, rates []*shipping.Rate, currency model.CurrencyEnum) *model.ShippingCartAddress {
	addr := &model.ShippingCartAddress{
		Firstname: a.Firstname,
		Lastname:  a.Lastname,
		Street:    ToStringPtrs(strings.Split(a.Street, "\n")),
		City:      a.City,
		Postcode:  a.Postcode,
		Company:   a.Company,
		Telephone: a.Telephone,
		Country:   &model.CartAddressCountry{Code: a.CountryID, Label: countryLabel(a.CountryID)},
	}
	if a.RegionID != nil {
		code, name, err := m.addressRepo.ResolveRegion(ctx, *a.RegionID)
		if err == nil {
			addr.Region = &model.CartAddressRegion{Code: &code, Label: &name, RegionID: a.RegionID}
		}
	} else if a.Region != nil {
		addr.Region = &model.CartAddressRegion{Label: a.Region}
	}
	if a.ShippingMethod != nil {
		parts := strings.SplitN(*a.ShippingMethod, "_", 2)
		if len(parts) == 2 {
			// Resolve human-readable titles from the already-collected rates,
			// which were produced by the carrier using its config values.
			carrierTitle := parts[0]
			methodTitle := parts[1]
			for _, r := range rates {
				if r.CarrierCode == parts[0] && r.MethodCode == parts[1] {
					carrierTitle = r.CarrierTitle
					methodTitle = r.MethodTitle
					break
				}
			}
			shippingMoney := &model.Money{Value: &a.ShippingAmount, Currency: nil}
			addr.SelectedShippingMethod = &model.SelectedShippingMethod{
				CarrierCode:  parts[0],
				CarrierTitle: carrierTitle,
				MethodCode:   parts[1],
				MethodTitle:  methodTitle,
				Amount:       shippingMoney,
				PriceExclTax: shippingMoney,
				PriceInclTax: shippingMoney,
			}
		}
	}
	for _, r := range rates {
		price := r.Price
		priceMoney := &model.Money{Value: &price, Currency: &currency}
		addr.AvailableShippingMethods = append(addr.AvailableShippingMethods, &model.AvailableShippingMethod{
			CarrierCode:  r.CarrierCode,
			CarrierTitle: r.CarrierTitle,
			MethodCode:   r.MethodCode,
			MethodTitle:  r.MethodTitle,
			Amount:       priceMoney,
			PriceExclTax: priceMoney,
			PriceInclTax: priceMoney,
			Available:    true,
		})
	}
	return addr
}

// MapBillingAddress converts a billing CartAddressData into the GraphQL type.
func (m *CartMapper) MapBillingAddress(ctx context.Context, a *repository.CartAddressData) *model.BillingCartAddress {
	addr := &model.BillingCartAddress{
		Firstname: a.Firstname,
		Lastname:  a.Lastname,
		Street:    ToStringPtrs(strings.Split(a.Street, "\n")),
		City:      a.City,
		Postcode:  a.Postcode,
		Company:   a.Company,
		Telephone: a.Telephone,
		Country:   &model.CartAddressCountry{Code: a.CountryID, Label: countryLabel(a.CountryID)},
	}
	if a.RegionID != nil {
		code, name, err := m.addressRepo.ResolveRegion(ctx, *a.RegionID)
		if err == nil {
			addr.Region = &model.CartAddressRegion{Code: &code, Label: &name, RegionID: a.RegionID}
		}
	}
	return addr
}

// MapCartItem converts a CartItemData into the appropriate GraphQL cart item interface.
// rowTotalInclTax must be pre-computed by the caller (MapCart) from the pipeline totals.
// thumbs is a map of productID → thumbnail image (may be nil or empty).
func (m *CartMapper) MapCartItem(ctx context.Context, item *repository.CartItemData, allItems []*repository.CartItemData, currency model.CurrencyEnum, rowTotalInclTax float64, thumbs map[int]*model.CartItemProductImage, urlKeys map[int]string, customOpts map[int][]*model.SelectedCustomizableOption) model.CartItemInterface {
	uid := EncodeUID(item.ItemID)
	prices := &model.CartItemPrices{
		Price:                &model.Money{Value: &item.Price, Currency: &currency},
		RowTotal:             &model.Money{Value: &item.RowTotal, Currency: &currency},
		RowTotalIncludingTax: &model.Money{Value: &rowTotalInclTax, Currency: &currency},
	}
	if item.DiscountAmount > 0 {
		itemDiscount := model.DiscountAppliedToTypeItem
		prices.TotalItemDiscount = &model.Money{Value: &item.DiscountAmount, Currency: &currency}
		prices.Discounts = []*model.Discount{{
			Amount:    &model.Money{Value: &item.DiscountAmount, Currency: &currency},
			Label:     "Discount",
			AppliedTo: &itemDiscount,
		}}
	}

	opts := customOpts[item.ItemID]
	if opts == nil {
		opts = []*model.SelectedCustomizableOption{}
	}

	switch item.ProductType {
	case "configurable":
		return m.mapConfigurableItem(ctx, item, allItems, uid, prices, currency, thumbs, urlKeys, opts)
	case "bundle":
		return m.mapBundleItem(ctx, item, allItems, uid, prices, currency, thumbs, urlKeys, opts)
	default:
		urlKey := urlKeys[item.ProductID]
		var urlKeyPtr *string
		if urlKey != "" {
			urlKeyPtr = &urlKey
		}
		return &model.SimpleCartItem{
			UID:      uid,
			Quantity: item.Qty,
			Product: &model.CartItemProduct{
				Sku:       item.SKU,
				Name:      &item.Name,
				URLKey:    urlKeyPtr,
				Thumbnail: thumbs[item.ProductID],
			},
			Prices:               prices,
			CustomizableOptions: opts,
		}
	}
}

func (m *CartMapper) mapConfigurableItem(ctx context.Context, item *repository.CartItemData, allItems []*repository.CartItemData, uid string, prices *model.CartItemPrices, currency model.CurrencyEnum, thumbs map[int]*model.CartItemProductImage, urlKeys map[int]string, customOpts []*model.SelectedCustomizableOption) *model.ConfigurableCartItem {
	// Find the parent product's original SKU
	var parentSKU string
	m.db.QueryRowContext(ctx,
		"SELECT sku FROM catalog_product_entity WHERE entity_id = ?",
		item.ProductID,
	).Scan(&parentSKU)

	// Find child item
	var childItem *repository.CartItemData
	for _, ci := range allItems {
		if ci.ParentItemID != nil && *ci.ParentItemID == item.ItemID {
			childItem = ci
			break
		}
	}

	// Resolve configurable options from super attributes
	var configOptions []*model.SelectedConfigurableOption
	rows, err := m.db.QueryContext(ctx, `
		SELECT cpsa.attribute_id, ea.attribute_code
		FROM catalog_product_super_attribute cpsa
		JOIN eav_attribute ea ON cpsa.attribute_id = ea.attribute_id
		WHERE cpsa.product_id = ?
		ORDER BY cpsa.position`, item.ProductID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var attrID int
			var attrCode string
			rows.Scan(&attrID, &attrCode)

			if childItem != nil {
				var optionID int
				m.db.QueryRowContext(ctx,
					"SELECT value FROM catalog_product_entity_int WHERE entity_id = ? AND attribute_id = ? AND store_id = 0",
					childItem.ProductID, attrID,
				).Scan(&optionID)

				var optionLabel string
				m.db.QueryRowContext(ctx,
					"SELECT value FROM eav_attribute_option_value WHERE option_id = ? AND store_id = 0",
					optionID,
				).Scan(&optionLabel)

				var attrLabel string
				m.db.QueryRowContext(ctx,
					"SELECT COALESCE(frontend_label, attribute_code) FROM eav_attribute WHERE attribute_id = ?",
					attrID,
				).Scan(&attrLabel)

				configOptions = append(configOptions, &model.SelectedConfigurableOption{
					ID:          attrID,
					OptionLabel: attrLabel,
					ValueID:     optionID,
					ValueLabel:  optionLabel,
				})
			}
		}
	}

	parentURLKey := urlKeys[item.ProductID]
	var parentURLKeyPtr *string
	if parentURLKey != "" {
		parentURLKeyPtr = &parentURLKey
	}
	result := &model.ConfigurableCartItem{
		UID:      uid,
		Quantity: item.Qty,
		Product: &model.CartItemProduct{
			Sku:       parentSKU,
			Name:      &item.Name,
			URLKey:    parentURLKeyPtr,
			Thumbnail: thumbs[item.ProductID],
		},
		Prices:              prices,
		ConfigurableOptions: configOptions,
		CustomizableOptions: customOpts,
	}
	if childItem != nil {
		childURLKey := urlKeys[childItem.ProductID]
		var childURLKeyPtr *string
		if childURLKey != "" {
			childURLKeyPtr = &childURLKey
		}
		result.ConfiguredVariant = &model.CartItemProduct{
			Sku:       childItem.SKU,
			Name:      &childItem.Name,
			URLKey:    childURLKeyPtr,
			Thumbnail: thumbs[childItem.ProductID],
		}
	}
	return result
}

func (m *CartMapper) mapBundleItem(ctx context.Context, item *repository.CartItemData, allItems []*repository.CartItemData, uid string, prices *model.CartItemPrices, currency model.CurrencyEnum, thumbs map[int]*model.CartItemProductImage, urlKeys map[int]string, customOpts []*model.SelectedCustomizableOption) *model.BundleCartItem {
	// Find child items
	var childItems []*repository.CartItemData
	for _, ci := range allItems {
		if ci.ParentItemID != nil && *ci.ParentItemID == item.ItemID {
			childItems = append(childItems, ci)
		}
	}

	// Look up bundle options
	var bundleOptions []*model.SelectedBundleOption
	rows, err := m.db.QueryContext(ctx, `
		SELECT bo.option_id, COALESCE(bov.title, ''), bo.type
		FROM catalog_product_bundle_option bo
		LEFT JOIN catalog_product_bundle_option_value bov ON bo.option_id = bov.option_id AND bov.store_id = 0
		WHERE bo.parent_id = ?
		ORDER BY bo.position`, item.ProductID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var optionID int
			var title, optType string
			rows.Scan(&optionID, &title, &optType)

			var values []*model.SelectedBundleOptionValue
			for _, child := range childItems {
				var selectionID int
				var selQty float64
				err := m.db.QueryRowContext(ctx,
					"SELECT selection_id, selection_qty FROM catalog_product_bundle_selection WHERE parent_product_id = ? AND option_id = ? AND product_id = ?",
					item.ProductID, optionID, child.ProductID,
				).Scan(&selectionID, &selQty)
				if err != nil {
					continue
				}
				childPrice := child.Price
				if childPrice == 0 {
					// look up from catalog
					m.db.QueryRowContext(ctx,
						"SELECT COALESCE(final_price, 0) FROM catalog_product_index_price WHERE entity_id = ? AND customer_group_id = 0 LIMIT 1",
						child.ProductID,
					).Scan(&childPrice)
				}
				values = append(values, &model.SelectedBundleOptionValue{
					ID:       selectionID,
					Label:    child.Name,
					Quantity: child.Qty / item.Qty,
					Price:    childPrice,
				})
			}

			if len(values) > 0 {
				optionUID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("bundle/%d", optionID)))
				bundleOptions = append(bundleOptions, &model.SelectedBundleOption{
					UID:    optionUID,
					Label:  title,
					Type:   optType,
					Values: values,
				})
			}
		}
	}

	var parentSKU string
	m.db.QueryRowContext(ctx,
		"SELECT sku FROM catalog_product_entity WHERE entity_id = ?",
		item.ProductID,
	).Scan(&parentSKU)

	bundleURLKey := urlKeys[item.ProductID]
	var bundleURLKeyPtr *string
	if bundleURLKey != "" {
		bundleURLKeyPtr = &bundleURLKey
	}
	return &model.BundleCartItem{
		UID:      uid,
		Quantity: item.Qty,
		Product: &model.CartItemProduct{
			Sku:       parentSKU,
			Name:      &item.Name,
			URLKey:    bundleURLKeyPtr,
			Thumbnail: thumbs[item.ProductID],
		},
		Prices:              prices,
		BundleOptions:       bundleOptions,
		CustomizableOptions: customOpts,
	}
}

// loadThumbnails batch-loads the small_image attribute value for the given product IDs.
// Returns a map of productID → image path (e.g. "/e/x/example.jpg").
// Values of "no_selection" are excluded. On any DB error the empty map is returned.
func (m *CartMapper) loadThumbnails(ctx context.Context, productIDs []int) map[int]string {
	if len(productIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(productIDs))
	args := make([]interface{}, len(productIDs))
	for i, id := range productIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT entity_id, value
		FROM catalog_product_entity_varchar
		WHERE attribute_id = (
			SELECT attribute_id FROM eav_attribute
			WHERE attribute_code = 'small_image' AND entity_type_id = 4
		)
		  AND store_id = 0
		  AND entity_id IN (%s)
		  AND value IS NOT NULL AND value != 'no_selection'`,
		strings.Join(placeholders, ","),
	)
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[int]string, len(productIDs))
	for rows.Next() {
		var entityID int
		var value string
		if rows.Scan(&entityID, &value) == nil {
			result[entityID] = value
		}
	}
	return result
}

// loadCustomizableOptions batch-loads selected custom options for the given item IDs.
// Returns a map of itemID → []*SelectedCustomizableOption.
// Items with no custom options are not present in the result map.
func (m *CartMapper) loadCustomizableOptions(ctx context.Context, itemIDs []int) map[int][]*model.SelectedCustomizableOption {
	if len(itemIDs) == 0 {
		return nil
	}

	// Build placeholders for the IN clause.
	ph := make([]string, len(itemIDs))
	args := make([]interface{}, len(itemIDs))
	for i, id := range itemIDs {
		ph[i] = "?"
		args[i] = id
	}
	inClause := strings.Join(ph, ",")

	// Load all quote_item_option rows for these items in one query.
	rows, err := m.db.QueryContext(ctx,
		fmt.Sprintf("SELECT item_id, code, COALESCE(value,'') FROM quote_item_option WHERE item_id IN (%s)", inClause),
		args...,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	// optionIDsByItem: item_id → []optionID (from option_ids rows)
	// valueByItemOption: (item_id, option_id) → raw value
	type itemOptKey struct{ itemID, optID int }
	optionIDsByItem := make(map[int][]int)
	valueByItemOption := make(map[itemOptKey]string)

	for rows.Next() {
		var itemID int
		var code, value string
		if rows.Scan(&itemID, &code, &value) != nil {
			continue
		}
		if code == "option_ids" {
			for _, idStr := range strings.Split(value, ",") {
				idStr = strings.TrimSpace(idStr)
				if id, err := strconv.Atoi(idStr); err == nil && id > 0 {
					optionIDsByItem[itemID] = append(optionIDsByItem[itemID], id)
				}
			}
		} else if strings.HasPrefix(code, "option_") {
			idStr := strings.TrimPrefix(code, "option_")
			if id, err := strconv.Atoi(idStr); err == nil {
				valueByItemOption[itemOptKey{itemID, id}] = value
			}
		}
	}

	if len(optionIDsByItem) == 0 {
		return nil
	}

	result := make(map[int][]*model.SelectedCustomizableOption)

	for itemID, optIDs := range optionIDsByItem {
		for _, optID := range optIDs {
			rawValue := valueByItemOption[itemOptKey{itemID, optID}]

			// Load option metadata.
			var optType string
			var isRequire bool
			var sortOrder int
			err := m.db.QueryRowContext(ctx,
				"SELECT COALESCE(type,''), is_require, sort_order FROM catalog_product_option WHERE option_id = ?",
				optID,
			).Scan(&optType, &isRequire, &sortOrder)
			if err != nil {
				continue
			}

			// Load option title (store 0 = global default).
			var optTitle string
			m.db.QueryRowContext(ctx,
				"SELECT COALESCE(title,'') FROM catalog_product_option_title WHERE option_id = ? AND store_id = 0",
				optID,
			).Scan(&optTitle)

			optUID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("custom-option/%d", optID)))

			var values []*model.SelectedCustomizableOptionValue

			isSelectType := optType == "drop_down" || optType == "radio" ||
				optType == "checkbox" || optType == "multiple"

			if isSelectType {
				// rawValue is comma-separated option_type_ids.
				for _, typeIDStr := range strings.Split(rawValue, ",") {
					typeIDStr = strings.TrimSpace(typeIDStr)
					typeID, err := strconv.Atoi(typeIDStr)
					if err != nil || typeID == 0 {
						continue
					}

					var typeTitle string
					m.db.QueryRowContext(ctx,
						"SELECT COALESCE(title,'') FROM catalog_product_option_type_title WHERE option_type_id = ? AND store_id = 0",
						typeID,
					).Scan(&typeTitle)

					var typePrice float64
					var typePriceType string
					m.db.QueryRowContext(ctx,
						"SELECT COALESCE(price,0), COALESCE(price_type,'fixed') FROM catalog_product_option_type_price WHERE option_type_id = ? AND store_id = 0",
						typeID,
					).Scan(&typePrice, &typePriceType)

					priceEnum := priceTypeEnum(typePriceType)
					valUID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("custom-option/%d/%d", optID, typeID)))
					values = append(values, &model.SelectedCustomizableOptionValue{
						CustomizableOptionValueUID: valUID,
						ID:                         typeID,
						Label:                      typeTitle,
						Value:                      typeIDStr,
						Price: &model.CartItemSelectedOptionValuePrice{
							Type:  priceEnum,
							Units: "$",
							Value: typePrice,
						},
					})
				}
			} else {
				// Text, area, date, file — value is the raw input.
				var optPrice float64
				var optPriceType string
				m.db.QueryRowContext(ctx,
					"SELECT COALESCE(price,0), COALESCE(price_type,'fixed') FROM catalog_product_option_price WHERE option_id = ? AND store_id = 0",
					optID,
				).Scan(&optPrice, &optPriceType)

				priceEnum := priceTypeEnum(optPriceType)
				valUID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("custom-option/%d/%s", optID, rawValue)))
				values = append(values, &model.SelectedCustomizableOptionValue{
					CustomizableOptionValueUID: valUID,
					ID:                         optID,
					Label:                      rawValue,
					Value:                      rawValue,
					Price: &model.CartItemSelectedOptionValuePrice{
						Type:  priceEnum,
						Units: "$",
						Value: optPrice,
					},
				})
			}

			if len(values) == 0 {
				continue
			}
			result[itemID] = append(result[itemID], &model.SelectedCustomizableOption{
				CustomizableOptionUID: optUID,
				ID:                   optID,
				IsRequired:           isRequire,
				Label:                optTitle,
				SortOrder:            sortOrder,
				Type:                 optType,
				Values:               values,
			})
		}
	}
	return result
}

// priceTypeEnum converts a Magento price_type string to the GraphQL enum value.
func priceTypeEnum(s string) model.PriceTypeEnum {
	switch strings.ToLower(s) {
	case "percent":
		return model.PriceTypeEnumPercent
	case "dynamic":
		return model.PriceTypeEnumDynamic
	default:
		return model.PriceTypeEnumFixed
	}
}

// loadURLKeys batch-loads the url_key EAV attribute for the given product IDs.
// Returns a map of productID → url_key string (e.g. "my-product").
// On any DB error the empty map is returned.
func (m *CartMapper) loadURLKeys(ctx context.Context, productIDs []int) map[int]string {
	if len(productIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(productIDs))
	args := make([]interface{}, len(productIDs))
	for i, id := range productIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT entity_id, value
		FROM catalog_product_entity_varchar
		WHERE attribute_id = (
			SELECT attribute_id FROM eav_attribute
			WHERE attribute_code = 'url_key' AND entity_type_id = 4
		)
		  AND store_id = 0
		  AND entity_id IN (%s)
		  AND value IS NOT NULL`,
		strings.Join(placeholders, ","),
	)
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[int]string, len(productIDs))
	for rows.Next() {
		var entityID int
		var value string
		if rows.Scan(&entityID, &value) == nil {
			result[entityID] = value
		}
	}
	return result
}

// EncodeUID encodes an integer item ID as a base64 UID.
func EncodeUID(id int) string {
	return base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(id)))
}

// DecodeUID decodes a base64 UID back to an integer item ID.
func DecodeUID(uid string) (int, error) {
	decoded, err := base64.StdEncoding.DecodeString(uid)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(decoded))
}

// ToStringPtrs converts a string slice to a slice of string pointers.
func ToStringPtrs(ss []string) []*string {
	result := make([]*string, len(ss))
	for i := range ss {
		result[i] = &ss[i]
	}
	return result
}

// itemRowTotalInclTax computes the tax-inclusive row total for a single cart item.
// For inclusive-price stores, the row total already contains tax.
// Otherwise, the item's share of product tax is looked up from pipeline results.
func itemRowTotalInclTax(itemID int, rowTotal float64, taxIncludedInPrice bool, dt *totals.Total) float64 {
	if taxIncludedInPrice {
		return rowTotal
	}
	if dt != nil {
		if tax := dt.ItemTaxes[itemID]; tax > 0 {
			return math.Round((rowTotal+tax)*100) / 100
		}
	}
	return rowTotal
}

// countryLabel returns the English display name for an ISO 3166-1 alpha-2 country
// code (e.g. "US" → "United States"). Falls back to the code on unknown input.
func countryLabel(code string) string {
	region, err := language.ParseRegion(code)
	if err != nil {
		return code
	}
	name := display.English.Regions().Name(region)
	if name == "" {
		return code
	}
	return name
}

// subtotalExclTax returns a pointer to the tax-exclusive subtotal.
func subtotalExclTax(subtotal float64, t *totals.Total) *model.Money {
	var excl float64
	if t != nil && t.TaxIncludedInPrice {
		excl = subtotal - (t.TaxAmount - t.ShippingTaxAmount)
	} else {
		excl = subtotal
	}
	return &model.Money{Value: &excl}
}
