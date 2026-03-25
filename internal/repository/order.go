package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

type OrderRepository struct {
	db *sql.DB
}

func NewOrderRepository(db *sql.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

// PlaceOrder converts a quote into an order. All operations are in a single transaction.
// Returns the order increment_id (e.g., "000000003").
func (r *OrderRepository) PlaceOrder(ctx context.Context, cart *CartData, items []*CartItemData, addrs []*CartAddressData, paymentMethod string, taxAmount float64) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// 1. Reserve order increment_id
	result, err := tx.ExecContext(ctx, "INSERT INTO sequence_order_1 VALUES ()")
	if err != nil {
		return "", fmt.Errorf("reserve order number: %w", err)
	}
	seqVal, _ := result.LastInsertId()
	incrementID := fmt.Sprintf("%09d", seqVal)

	// Find addresses
	var shippingAddr, billingAddr *CartAddressData
	for _, a := range addrs {
		switch a.AddressType {
		case "shipping":
			shippingAddr = a
		case "billing":
			billingAddr = a
		}
	}
	if billingAddr == nil && shippingAddr != nil {
		billingAddr = shippingAddr
	}

	// Compute totals
	var shippingAmount float64
	shippingMethod := ""
	shippingDescription := ""
	if shippingAddr != nil {
		shippingAmount = shippingAddr.ShippingAmount
		if shippingAddr.ShippingMethod != nil {
			shippingMethod = *shippingAddr.ShippingMethod
		}
		if shippingAddr.ShippingDescription != nil {
			shippingDescription = *shippingAddr.ShippingDescription
		}
	}

	var totalQty float64
	var totalItemCount int
	for _, item := range items {
		if item.ParentItemID == nil {
			totalQty += item.Qty
			totalItemCount++
		}
	}

	protectCode := generateProtectCode()
	email := ""
	if cart.CustomerEmail != nil {
		email = *cart.CustomerEmail
	}
	customerFirstname := ""
	customerLastname := ""
	if billingAddr != nil {
		customerFirstname = billingAddr.Firstname
		customerLastname = billingAddr.Lastname
	}

	subtotalInclTax := cart.Subtotal + taxAmount
	shippingInclTax := shippingAmount // no tax on shipping yet
	customerGroupID := cart.CustomerGroupID

	// Compute discount from cart totals
	discountAmount := 0.0
	if cart.SubtotalWithDiscount > 0 && cart.SubtotalWithDiscount < cart.Subtotal {
		discountAmount = cart.Subtotal - cart.SubtotalWithDiscount
	}

	// 2. Insert sales_order
	orderResult, err := tx.ExecContext(ctx, `
		INSERT INTO sales_order (
			state, status, store_id, store_name,
			customer_id, customer_is_guest, customer_group_id, customer_email,
			customer_firstname, customer_lastname, customer_note_notify,
			increment_id, quote_id, coupon_code, applied_rule_ids,
			is_virtual, shipping_method, shipping_description,
			base_grand_total, grand_total, base_subtotal, subtotal,
			subtotal_incl_tax, base_subtotal_incl_tax,
			base_tax_amount, tax_amount,
			base_shipping_amount, shipping_amount,
			shipping_incl_tax, base_shipping_incl_tax,
			base_shipping_tax_amount, shipping_tax_amount,
			base_discount_amount, discount_amount,
			base_shipping_discount_amount, shipping_discount_amount,
			discount_tax_compensation_amount, base_discount_tax_compensation_amount,
			shipping_discount_tax_compensation_amount, base_shipping_discount_tax_compensation_amnt,
			total_qty_ordered, total_item_count,
			base_total_due, total_due, weight,
			base_currency_code, order_currency_code, global_currency_code, store_currency_code,
			base_to_global_rate, base_to_order_rate, store_to_base_rate, store_to_order_rate,
			protect_code, send_email, created_at, updated_at
		) VALUES (
			'new', 'pending', ?, 'Main Website\nMain Website Store\nDefault Store View',
			?, ?, ?, ?,
			?, ?, 1,
			?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?, ?,
			?, ?,
			?, ?,
			?, ?,
			?, ?,
			0, 0,
			0, 0,
			0, 0,
			0, 0,
			?, ?,
			?, ?, 0,
			?, ?, 'USD', 'USD',
			1, 1, 0, 0,
			?, 1, NOW(), NOW()
		)`,
		cart.StoreID,
		cart.CustomerID, cart.CustomerIsGuest, customerGroupID, email,
		customerFirstname, customerLastname,
		incrementID, cart.EntityID, cart.CouponCode, cart.CouponCode,
		cart.IsVirtual, shippingMethod, shippingDescription,
		cart.GrandTotal, cart.GrandTotal, cart.Subtotal, cart.Subtotal,
		subtotalInclTax, subtotalInclTax,
		taxAmount, taxAmount,
		shippingAmount, shippingAmount,
		shippingInclTax, shippingInclTax,
		discountAmount, discountAmount,
		totalQty, totalItemCount,
		cart.GrandTotal, cart.GrandTotal,
		cart.BaseCurrencyCode, cart.QuoteCurrencyCode,
		protectCode,
	)
	if err != nil {
		return "", fmt.Errorf("insert sales_order: %w", err)
	}
	orderID, _ := orderResult.LastInsertId()

	// 3. Insert sales_order_item
	for _, item := range items {
		priceInclTax := item.Price + item.TaxAmount/item.Qty
		rowTotalInclTax := item.RowTotal + item.TaxAmount

		_, err := tx.ExecContext(ctx, `
			INSERT INTO sales_order_item (
				order_id, parent_item_id, quote_item_id,
				product_id, product_type, sku, name,
				qty_ordered, price, base_price, original_price, base_original_price,
				price_incl_tax, base_price_incl_tax,
				row_total, base_row_total,
				row_total_incl_tax, base_row_total_incl_tax,
				tax_percent, tax_amount, base_tax_amount,
				discount_percent, discount_amount, base_discount_amount,
				discount_tax_compensation_amount, base_discount_tax_compensation_amount,
				is_virtual, store_id, created_at, updated_at
			) VALUES (
				?, NULL, ?,
				?, ?, ?, ?,
				?, ?, ?, ?, ?,
				?, ?,
				?, ?,
				?, ?,
				?, ?, ?,
				?, ?, ?,
				0, 0,
				0, ?, NOW(), NOW()
			)`,
			orderID, item.ItemID,
			item.ProductID, item.ProductType, item.SKU, item.Name,
			item.Qty, item.Price, item.Price, item.Price, item.Price,
			priceInclTax, priceInclTax,
			item.RowTotal, item.RowTotal,
			rowTotalInclTax, rowTotalInclTax,
			item.TaxPercent, item.TaxAmount, item.TaxAmount,
			item.DiscountPercent(), item.DiscountAmount, item.DiscountAmount,
			cart.StoreID,
		)
		if err != nil {
			return "", fmt.Errorf("insert order item %s: %w", item.SKU, err)
		}
	}

	// 4. Insert sales_order_address (billing + shipping) and collect IDs
	var billingAddrID, shippingAddrID int64
	for _, addr := range []*CartAddressData{billingAddr, shippingAddr} {
		if addr == nil {
			continue
		}
		addrResult, err := tx.ExecContext(ctx, `
			INSERT INTO sales_order_address (
				parent_id, address_type, quote_address_id, customer_id, email,
				firstname, lastname, company, street, city,
				region, region_id, postcode, country_id, telephone
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			orderID, addr.AddressType, addr.AddressID, cart.CustomerID, email,
			addr.Firstname, addr.Lastname, addr.Company, addr.Street, addr.City,
			addr.Region, addr.RegionID, addr.Postcode, addr.CountryID, addr.Telephone,
		)
		if err != nil {
			return "", fmt.Errorf("insert order address (%s): %w", addr.AddressType, err)
		}
		addrID, _ := addrResult.LastInsertId()
		if addr.AddressType == "billing" {
			billingAddrID = addrID
		} else {
			shippingAddrID = addrID
		}
	}

	// Backfill address IDs on sales_order
	_, err = tx.ExecContext(ctx,
		"UPDATE sales_order SET billing_address_id = ?, shipping_address_id = ? WHERE entity_id = ?",
		billingAddrID, shippingAddrID, orderID,
	)
	if err != nil {
		return "", fmt.Errorf("update order address ids: %w", err)
	}

	// 5. Insert sales_order_payment
	_, err = tx.ExecContext(ctx, `
		INSERT INTO sales_order_payment (
			parent_id, method, amount_ordered, base_amount_ordered,
			shipping_amount, base_shipping_amount,
			additional_information
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		orderID, paymentMethod, cart.GrandTotal, cart.GrandTotal,
		shippingAmount, shippingAmount,
		fmt.Sprintf(`{"method_title":"%s"}`, paymentMethod),
	)
	if err != nil {
		return "", fmt.Errorf("insert order payment: %w", err)
	}

	// 6. Insert sales_order_grid
	customerName := customerFirstname + " " + customerLastname
	billingAddrStr := ""
	shippingAddrStr := ""
	if billingAddr != nil {
		billingAddrStr = formatGridAddressString(billingAddr)
	}
	if shippingAddr != nil {
		shippingAddrStr = formatGridAddressString(shippingAddr)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO sales_order_grid (
			entity_id, increment_id, status, store_id, store_name,
			customer_id, customer_email, customer_group, customer_name,
			billing_name, shipping_name,
			billing_address, shipping_address,
			shipping_information, payment_method,
			base_grand_total, grand_total,
			subtotal, shipping_and_handling,
			base_currency_code, order_currency_code,
			created_at, updated_at
		) VALUES (?, ?, 'pending', ?, 'Main Website\nMain Website Store\nDefault Store View',
			?, ?, 0, ?,
			?, ?,
			?, ?,
			?, ?,
			?, ?,
			?, ?,
			?, ?,
			NOW(), NOW()
		)`,
		orderID, incrementID, cart.StoreID,
		cart.CustomerID, email, customerName,
		customerName, customerName,
		billingAddrStr, shippingAddrStr,
		shippingDescription, paymentMethod,
		cart.GrandTotal, cart.GrandTotal,
		cart.Subtotal, shippingAmount,
		cart.BaseCurrencyCode, cart.QuoteCurrencyCode,
	)
	if err != nil {
		return "", fmt.Errorf("insert order grid: %w", err)
	}

	// 7. Insert inventory_reservation (negative qty per SKU)
	for _, item := range items {
		if item.ParentItemID != nil {
			continue
		}
		metadata := fmt.Sprintf(`{"event_type":"order_placed","object_type":"order","object_id":"%d","object_increment_id":"%s"}`, orderID, incrementID)
		_, err := tx.ExecContext(ctx,
			"INSERT INTO inventory_reservation (stock_id, sku, quantity, metadata) VALUES (1, ?, ?, ?)",
			item.SKU, -item.Qty, metadata,
		)
		if err != nil {
			return "", fmt.Errorf("insert inventory reservation for %s: %w", item.SKU, err)
		}
	}

	// 8. Deactivate quote
	_, err = tx.ExecContext(ctx,
		"UPDATE quote SET is_active = 0, reserved_order_id = ?, updated_at = NOW() WHERE entity_id = ?",
		incrementID, cart.EntityID,
	)
	if err != nil {
		return "", fmt.Errorf("deactivate quote: %w", err)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit order: %w", err)
	}

	return incrementID, nil
}

// formatGridAddressString matches Magento's grid address format (no name, no country, comma-separated).
func formatGridAddressString(a *CartAddressData) string {
	var parts []string
	if a.Street != "" {
		parts = append(parts, strings.Replace(a.Street, "\n", ", ", -1))
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

func generateProtectCode() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
