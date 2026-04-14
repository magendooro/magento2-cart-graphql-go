package order

import (
	"strings"

	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
	"github.com/magendooro/magento2-cart-graphql-go/internal/totals"
)

// CartToOrder converts cart data and computed totals into an OrderInput ready
// for Place. This is a pure function — no DB access, no side effects.
func CartToOrder(
	cart *repository.CartData,
	items []*repository.CartItemData,
	addrs []*repository.CartAddressData,
	paymentMethod string,
	t *totals.Total,
) OrderInput {
	var billingAddr, shippingAddr *repository.CartAddressData
	for _, a := range addrs {
		switch a.AddressType {
		case "billing":
			billingAddr = a
		case "shipping":
			shippingAddr = a
		}
	}
	if billingAddr == nil && shippingAddr != nil {
		billingAddr = shippingAddr
	}

	email := ""
	if cart.CustomerEmail != nil {
		email = *cart.CustomerEmail
	}

	firstname, lastname := "", ""
	if billingAddr != nil {
		firstname = billingAddr.Firstname
		lastname = billingAddr.Lastname
	}

	shippingMethod, shippingDescription := "", ""
	shippingAmount := 0.0
	if shippingAddr != nil {
		shippingAmount = shippingAddr.ShippingAmount
		if shippingAddr.ShippingMethod != nil {
			shippingMethod = *shippingAddr.ShippingMethod
		}
		if shippingAddr.ShippingDescription != nil {
			shippingDescription = *shippingAddr.ShippingDescription
		}
	}

	var taxAmount, shippingTaxAmount, discountAmount float64
	if t != nil {
		taxAmount = t.TaxAmount
		shippingTaxAmount = t.ShippingTaxAmount
		discountAmount = t.DiscountAmount
	}
	shippingInclTax := shippingAmount + shippingTaxAmount
	subtotalInclTax := cart.Subtotal + taxAmount - shippingTaxAmount

	var totalQty float64
	var totalItemCount int
	for _, item := range items {
		if item.ParentItemID == nil {
			totalQty += item.Qty
			totalItemCount++
		}
	}

	orderItems := make([]OrderItemInput, len(items))
	for i, item := range items {
		priceInclTax := item.Price + item.TaxAmount/item.Qty
		rowTotalInclTax := item.RowTotal + item.TaxAmount
		orderItems[i] = OrderItemInput{
			QuoteItemID:       item.ItemID,
			QuoteParentItemID: item.ParentItemID,
			ProductID:         item.ProductID,
			ProductType:       item.ProductType,
			SKU:               item.SKU,
			Name:              item.Name,
			Qty:               item.Qty,
			Price:             item.Price,
			RowTotal:          item.RowTotal,
			PriceInclTax:      priceInclTax,
			RowTotalInclTax:   rowTotalInclTax,
			TaxPercent:        item.TaxPercent,
			TaxAmount:         item.TaxAmount,
			DiscountPercent:   item.DiscountPercent(),
			DiscountAmount:    item.DiscountAmount,
			IsVirtual:         0,
			StoreID:           cart.StoreID,
		}
	}

	in := OrderInput{
		StoreID:           cart.StoreID,
		QuoteID:           cart.EntityID,
		CustomerID:        cart.CustomerID,
		CustomerIsGuest:   cart.CustomerIsGuest,
		CustomerGroupID:   cart.CustomerGroupID,
		CustomerEmail:     email,
		Firstname:         firstname,
		Lastname:          lastname,
		IsVirtual:         cart.IsVirtual,
		CouponCode:        cart.CouponCode,
		ShippingMethod:    shippingMethod,
		ShippingDescription: shippingDescription,
		BaseCurrencyCode:  cart.BaseCurrencyCode,
		OrderCurrencyCode: cart.QuoteCurrencyCode,
		Subtotal:          cart.Subtotal,
		SubtotalInclTax:   subtotalInclTax,
		ShippingAmount:    shippingAmount,
		ShippingTaxAmount: shippingTaxAmount,
		ShippingInclTax:   shippingInclTax,
		TaxAmount:         taxAmount,
		DiscountAmount:    discountAmount,
		GrandTotal:        cart.GrandTotal,
		TotalQty:          totalQty,
		TotalItemCount:    totalItemCount,
		Items:             orderItems,
		BillingAddr:       toOrderAddress(billingAddr, email, cart.CustomerID),
		ShippingAddr:      toOrderAddress(shippingAddr, email, cart.CustomerID),
		PaymentMethod:     paymentMethod,
	}
	return in
}

func toOrderAddress(a *repository.CartAddressData, email string, customerID *int) *OrderAddressInput {
	if a == nil {
		return nil
	}
	return &OrderAddressInput{
		AddressType: a.AddressType,
		QuoteAddrID: a.AddressID,
		CustomerID:  customerID,
		Email:       email,
		Firstname:   a.Firstname,
		Lastname:    a.Lastname,
		Company:     a.Company,
		Street:      a.Street,
		City:        a.City,
		Region:      a.Region,
		RegionID:    a.RegionID,
		Postcode:    a.Postcode,
		CountryID:   a.CountryID,
		Telephone:   a.Telephone,
	}
}

// FormatGridAddress matches Magento's admin grid address format:
// street,city,region,postcode — no name, no country, comma without spaces.
func FormatGridAddress(a *OrderAddressInput) string {
	if a == nil {
		return ""
	}
	var parts []string
	if a.Street != "" {
		parts = append(parts, strings.ReplaceAll(a.Street, "\n", ", "))
	}
	parts = append(parts, a.City)
	if a.Region != nil {
		parts = append(parts, *a.Region)
	}
	if a.Postcode != nil {
		parts = append(parts, *a.Postcode)
	}
	return strings.Join(parts, ",")
}
