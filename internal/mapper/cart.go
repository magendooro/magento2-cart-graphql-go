package mapper

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"

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
}

// MapCart produces the full GraphQL Cart object from pre-fetched service data.
func (m *CartMapper) MapCart(ctx context.Context, in MapCartInput) *model.Cart {
	currency := model.CurrencyEnum(in.Cart.QuoteCurrencyCode)

	cartItems := make([]model.CartItemInterface, 0, len(in.Items))
	for _, item := range in.Items {
		if item.ParentItemID != nil {
			continue
		}
		cartItems = append(cartItems, m.MapCartItem(ctx, item, in.Items, currency))
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
	var productTaxAmount float64
	if in.DisplayTotals != nil {
		productTaxAmount = in.DisplayTotals.TaxAmount - in.DisplayTotals.ShippingTaxAmount
	}
	var subtotalInclTax float64
	if in.DisplayTotals != nil && in.DisplayTotals.TaxIncludedInPrice {
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
		discounts = append(discounts, &model.Discount{
			Amount: &model.Money{Value: &discountAmount, Currency: &currency},
			Label:  label,
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
		Country:   &model.CartAddressCountry{Code: a.CountryID, Label: a.CountryID},
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
			desc := ""
			if a.ShippingDescription != nil {
				desc = *a.ShippingDescription
			}
			addr.SelectedShippingMethod = &model.SelectedShippingMethod{
				CarrierCode:  parts[0],
				CarrierTitle: parts[0],
				MethodCode:   parts[1],
				MethodTitle:  desc,
				Amount:       &model.Money{Value: &a.ShippingAmount, Currency: nil},
			}
		}
	}
	for _, r := range rates {
		price := r.Price
		addr.AvailableShippingMethods = append(addr.AvailableShippingMethods, &model.AvailableShippingMethod{
			CarrierCode:  r.CarrierCode,
			CarrierTitle: r.CarrierTitle,
			MethodCode:   r.MethodCode,
			MethodTitle:  r.MethodTitle,
			Amount:       &model.Money{Value: &price, Currency: &currency},
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
		Country:   &model.CartAddressCountry{Code: a.CountryID, Label: a.CountryID},
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
func (m *CartMapper) MapCartItem(ctx context.Context, item *repository.CartItemData, allItems []*repository.CartItemData, currency model.CurrencyEnum) model.CartItemInterface {
	uid := EncodeUID(item.ItemID)
	prices := &model.CartItemPrices{
		Price:                &model.Money{Value: &item.Price, Currency: &currency},
		RowTotal:             &model.Money{Value: &item.RowTotal, Currency: &currency},
		RowTotalIncludingTax: &model.Money{Value: &item.RowTotal, Currency: &currency},
	}
	if item.DiscountAmount > 0 {
		prices.TotalItemDiscount = &model.Money{Value: &item.DiscountAmount, Currency: &currency}
		prices.Discounts = []*model.Discount{{
			Amount: &model.Money{Value: &item.DiscountAmount, Currency: &currency},
			Label:  "Discount",
		}}
	}

	switch item.ProductType {
	case "configurable":
		return m.mapConfigurableItem(ctx, item, allItems, uid, prices, currency)
	case "bundle":
		return m.mapBundleItem(ctx, item, allItems, uid, prices, currency)
	default:
		return &model.SimpleCartItem{
			UID:      uid,
			Quantity: item.Qty,
			Product: &model.CartItemProduct{
				Sku:  item.SKU,
				Name: &item.Name,
			},
			Prices: prices,
		}
	}
}

func (m *CartMapper) mapConfigurableItem(ctx context.Context, item *repository.CartItemData, allItems []*repository.CartItemData, uid string, prices *model.CartItemPrices, currency model.CurrencyEnum) *model.ConfigurableCartItem {
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

	result := &model.ConfigurableCartItem{
		UID:      uid,
		Quantity: item.Qty,
		Product: &model.CartItemProduct{
			Sku:  parentSKU,
			Name: &item.Name,
		},
		Prices:              prices,
		ConfigurableOptions: configOptions,
	}
	if childItem != nil {
		result.ConfiguredVariant = &model.CartItemProduct{
			Sku:  childItem.SKU,
			Name: &childItem.Name,
		}
	}
	return result
}

func (m *CartMapper) mapBundleItem(ctx context.Context, item *repository.CartItemData, allItems []*repository.CartItemData, uid string, prices *model.CartItemPrices, currency model.CurrencyEnum) *model.BundleCartItem {
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

	return &model.BundleCartItem{
		UID:      uid,
		Quantity: item.Qty,
		Product: &model.CartItemProduct{
			Sku:  parentSKU,
			Name: &item.Name,
		},
		Prices:        prices,
		BundleOptions: bundleOptions,
	}
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
