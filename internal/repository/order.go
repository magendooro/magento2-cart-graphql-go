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

	// 2. Insert sales_order
	orderResult, err := tx.ExecContext(ctx, `
		INSERT INTO sales_order (
			state, status, store_id, customer_id, customer_is_guest, customer_email,
			customer_firstname, customer_lastname,
			increment_id, quote_id, coupon_code, applied_rule_ids,
			is_virtual, shipping_method, shipping_description,
			base_grand_total, grand_total, base_subtotal, subtotal,
			base_tax_amount, tax_amount,
			base_shipping_amount, shipping_amount,
			base_discount_amount, discount_amount,
			total_qty_ordered, total_item_count,
			base_currency_code, order_currency_code, global_currency_code, store_currency_code,
			base_to_global_rate, base_to_order_rate, store_to_base_rate, store_to_order_rate,
			protect_code, send_email, created_at, updated_at
		) VALUES (
			'new', 'pending', ?, ?, ?, ?,
			?, ?,
			?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?, ?,
			?, ?,
			0, 0,
			?, ?,
			?, ?, 'USD', 'USD',
			1, 1, 1, 1,
			?, 1, NOW(), NOW()
		)`,
		cart.StoreID, cart.CustomerID, cart.CustomerIsGuest, email,
		customerFirstname, customerLastname,
		incrementID, cart.EntityID, cart.CouponCode, nil,
		cart.IsVirtual, shippingMethod, shippingDescription,
		cart.GrandTotal, cart.GrandTotal, cart.Subtotal, cart.Subtotal,
		taxAmount, taxAmount,
		shippingAmount, shippingAmount,
		totalQty, totalItemCount,
		cart.BaseCurrencyCode, cart.QuoteCurrencyCode,
		protectCode,
	)
	if err != nil {
		return "", fmt.Errorf("insert sales_order: %w", err)
	}
	orderID, _ := orderResult.LastInsertId()

	// 3. Insert sales_order_item
	for _, item := range items {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sales_order_item (
				order_id, parent_item_id, quote_item_id,
				product_id, product_type, sku, name,
				qty_ordered, price, base_price, original_price, base_original_price,
				row_total, base_row_total,
				tax_percent, tax_amount, base_tax_amount,
				discount_percent, discount_amount, base_discount_amount,
				is_virtual, store_id, created_at, updated_at
			) VALUES (
				?, NULL, ?,
				?, ?, ?, ?,
				?, ?, ?, ?, ?,
				?, ?,
				?, ?, ?,
				0, 0, 0,
				0, ?, NOW(), NOW()
			)`,
			orderID, item.ItemID,
			item.ProductID, item.ProductType, item.SKU, item.Name,
			item.Qty, item.Price, item.Price, item.Price, item.Price,
			item.RowTotal, item.RowTotal,
			item.TaxPercent, item.TaxAmount, item.TaxAmount,
			cart.StoreID,
		)
		if err != nil {
			return "", fmt.Errorf("insert order item %s: %w", item.SKU, err)
		}
	}

	// 4. Insert sales_order_address (billing + shipping)
	for _, addr := range []*CartAddressData{billingAddr, shippingAddr} {
		if addr == nil {
			continue
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sales_order_address (
				parent_id, address_type, email,
				firstname, lastname, company, street, city,
				region, region_id, postcode, country_id, telephone
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			orderID, addr.AddressType, email,
			addr.Firstname, addr.Lastname, addr.Company, addr.Street, addr.City,
			addr.Region, addr.RegionID, addr.Postcode, addr.CountryID, addr.Telephone,
		)
		if err != nil {
			return "", fmt.Errorf("insert order address (%s): %w", addr.AddressType, err)
		}
	}

	// 5. Insert sales_order_payment
	_, err = tx.ExecContext(ctx, `
		INSERT INTO sales_order_payment (
			parent_id, method, amount_ordered, base_amount_ordered,
			shipping_amount, base_shipping_amount
		) VALUES (?, ?, ?, ?, ?, ?)`,
		orderID, paymentMethod, cart.GrandTotal, cart.GrandTotal,
		shippingAmount, shippingAmount,
	)
	if err != nil {
		return "", fmt.Errorf("insert order payment: %w", err)
	}

	// 6. Insert sales_order_grid
	billingName := customerFirstname + " " + customerLastname
	billingAddrStr := ""
	shippingAddrStr := ""
	if billingAddr != nil {
		billingAddrStr = formatAddressString(billingAddr)
	}
	if shippingAddr != nil {
		shippingAddrStr = formatAddressString(shippingAddr)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO sales_order_grid (
			entity_id, increment_id, status, store_id, store_name,
			customer_id, customer_email, customer_group,
			billing_name, shipping_name,
			billing_address, shipping_address,
			shipping_information, payment_method,
			base_grand_total, grand_total,
			total_refunded,
			created_at, updated_at
		) VALUES (?, ?, 'pending', ?, 'Main Website\nMain Website Store\nDefault Store View',
			?, ?, 0,
			?, ?,
			?, ?,
			?, ?,
			?, ?,
			0,
			NOW(), NOW()
		)`,
		orderID, incrementID, cart.StoreID,
		cart.CustomerID, email,
		billingName, billingName,
		billingAddrStr, shippingAddrStr,
		shippingDescription, paymentMethod,
		cart.GrandTotal, cart.GrandTotal,
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

func formatAddressString(a *CartAddressData) string {
	parts := []string{a.Firstname + " " + a.Lastname}
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
	parts = append(parts, a.CountryID)
	return strings.Join(parts, ", ")
}

func generateProtectCode() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
