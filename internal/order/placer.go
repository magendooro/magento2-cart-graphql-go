package order

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// Place writes the order to the database inside a single transaction.
// It reserves the increment_id from sequence_order_1, inserts all order rows,
// deactivates the quote, and returns the increment_id and protect_code on success.
// The protect_code is the order token used by guestOrderByToken in the customer service.
func Place(ctx context.Context, db *sql.DB, in OrderInput) (incrementID, protectCode string, err error) {
	tx, txErr := db.BeginTx(ctx, nil)
	if txErr != nil {
		return "", "", fmt.Errorf("begin transaction: %w", txErr)
	}
	defer tx.Rollback()

	// 1. Reserve order increment_id
	res, err := tx.ExecContext(ctx, "INSERT INTO sequence_order_1 VALUES ()")
	if err != nil {
		return "", "", fmt.Errorf("reserve order number: %w", err)
	}
	seqVal, _ := res.LastInsertId()
	incrementID = fmt.Sprintf("%09d", seqVal)
	protectCode = generateProtectCode()

	// 2. Insert sales_order
	orderRes, err := tx.ExecContext(ctx, `
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
			protect_code, send_email, remote_ip, created_at, updated_at
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
			?, 1, ?, NOW(), NOW()
		)`,
		in.StoreID,
		in.CustomerID, in.CustomerIsGuest, in.CustomerGroupID, in.CustomerEmail,
		in.Firstname, in.Lastname,
		incrementID, in.QuoteID, in.CouponCode, in.CouponCode,
		in.IsVirtual, in.ShippingMethod, in.ShippingDescription,
		in.GrandTotal, in.GrandTotal, in.Subtotal, in.Subtotal,
		in.SubtotalInclTax, in.SubtotalInclTax,
		in.TaxAmount, in.TaxAmount,
		in.ShippingAmount, in.ShippingAmount,
		in.ShippingInclTax, in.ShippingInclTax,
		in.ShippingTaxAmount, in.ShippingTaxAmount,
		in.DiscountAmount, in.DiscountAmount,
		in.TotalQty, in.TotalItemCount,
		in.GrandTotal, in.GrandTotal,
		in.BaseCurrencyCode, in.OrderCurrencyCode,
		protectCode, in.RemoteIP,
	)
	if err != nil {
		return "", "", fmt.Errorf("insert sales_order: %w", err)
	}
	orderID, _ := orderRes.LastInsertId()

	// 3. Insert sales_order_item — two passes to resolve parent_item_id
	// First pass: insert top-level items (ParentItemID == nil) and record
	// quote_item_id → order_item_id mapping.
	quoteToOrderItemID := make(map[int]int64, len(in.Items))
	for _, item := range in.Items {
		if item.QuoteParentItemID != nil {
			continue
		}
		id, err := insertOrderItem(ctx, tx, orderID, 0, item)
		if err != nil {
			return "", "", err
		}
		quoteToOrderItemID[item.QuoteItemID] = id
	}
	// Second pass: insert child items, resolving parent_item_id.
	for _, item := range in.Items {
		if item.QuoteParentItemID == nil {
			continue
		}
		parentOrderItemID := quoteToOrderItemID[*item.QuoteParentItemID]
		if _, err := insertOrderItem(ctx, tx, orderID, parentOrderItemID, item); err != nil {
			return "", "", err
		}
	}

	// 4. Insert sales_order_address (billing + shipping) and collect IDs
	var billingAddrID, shippingAddrID int64
	for _, addr := range []*OrderAddressInput{in.BillingAddr, in.ShippingAddr} {
		if addr == nil {
			continue
		}
		addrRes, err := tx.ExecContext(ctx, `
			INSERT INTO sales_order_address (
				parent_id, address_type, quote_address_id, customer_id, email,
				firstname, lastname, company, street, city,
				region, region_id, postcode, country_id, telephone
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			orderID, addr.AddressType, addr.QuoteAddrID, addr.CustomerID, addr.Email,
			addr.Firstname, addr.Lastname, addr.Company, addr.Street, addr.City,
			addr.Region, addr.RegionID, addr.Postcode, addr.CountryID, addr.Telephone,
		)
		if err != nil {
			return "", "", fmt.Errorf("insert order address (%s): %w", addr.AddressType, err)
		}
		id, _ := addrRes.LastInsertId()
		if addr.AddressType == "billing" {
			billingAddrID = id
		} else {
			shippingAddrID = id
		}
	}

	// Backfill address IDs on sales_order
	if _, err := tx.ExecContext(ctx,
		"UPDATE sales_order SET billing_address_id = ?, shipping_address_id = ? WHERE entity_id = ?",
		billingAddrID, shippingAddrID, orderID,
	); err != nil {
		return "", "", fmt.Errorf("update order address ids: %w", err)
	}

	// 5. Insert sales_order_payment
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sales_order_payment (
			parent_id, method, amount_ordered, base_amount_ordered,
			shipping_amount, base_shipping_amount,
			additional_information
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		orderID, in.PaymentMethod, in.GrandTotal, in.GrandTotal,
		in.ShippingAmount, in.ShippingAmount,
		fmt.Sprintf(`{"method_title":"%s"}`, in.PaymentMethod),
	); err != nil {
		return "", "", fmt.Errorf("insert order payment: %w", err)
	}

	// 6. Insert sales_order_grid
	customerName := in.Firstname + " " + in.Lastname
	if _, err := tx.ExecContext(ctx, `
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
		orderID, incrementID, in.StoreID,
		in.CustomerID, in.CustomerEmail, customerName,
		customerName, customerName,
		FormatGridAddress(in.BillingAddr), FormatGridAddress(in.ShippingAddr),
		in.ShippingDescription, in.PaymentMethod,
		in.GrandTotal, in.GrandTotal,
		in.Subtotal, in.ShippingAmount,
		in.BaseCurrencyCode, in.OrderCurrencyCode,
	); err != nil {
		return "", "", fmt.Errorf("insert order grid: %w", err)
	}

	// 7. Insert inventory_reservation (negative qty per top-level SKU)
	for _, item := range in.Items {
		if item.QuoteParentItemID != nil {
			continue
		}
		metadata := fmt.Sprintf(
			`{"event_type":"order_placed","object_type":"order","object_id":"%d","object_increment_id":"%s"}`,
			orderID, incrementID,
		)
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO inventory_reservation (stock_id, sku, quantity, metadata) VALUES (1, ?, ?, ?)",
			item.SKU, -item.Qty, metadata,
		); err != nil {
			return "", "", fmt.Errorf("insert inventory reservation for %s: %w", item.SKU, err)
		}
	}

	// 8. Deactivate quote
	if _, err := tx.ExecContext(ctx,
		"UPDATE quote SET is_active = 0, reserved_order_id = ?, updated_at = NOW() WHERE entity_id = ?",
		incrementID, in.QuoteID,
	); err != nil {
		return "", "", fmt.Errorf("deactivate quote: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("commit order: %w", err)
	}

	return incrementID, protectCode, nil
}

func insertOrderItem(ctx context.Context, tx *sql.Tx, orderID int64, parentOrderItemID int64, item OrderItemInput) (int64, error) {
	var parentID interface{}
	if parentOrderItemID != 0 {
		parentID = parentOrderItemID
	}
	var productOptionsArg interface{}
	if item.ProductOptions != "" {
		productOptionsArg = item.ProductOptions
	}
	res, err := tx.ExecContext(ctx, `
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
			is_virtual, store_id, product_options, created_at, updated_at
		) VALUES (
			?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?,
			?, ?,
			?, ?,
			?, ?, ?,
			?, ?, ?,
			0, 0,
			0, ?, ?, NOW(), NOW()
		)`,
		orderID, parentID, item.QuoteItemID,
		item.ProductID, item.ProductType, item.SKU, item.Name,
		item.Qty, item.Price, item.Price, item.Price, item.Price,
		item.PriceInclTax, item.PriceInclTax,
		item.RowTotal, item.RowTotal,
		item.RowTotalInclTax, item.RowTotalInclTax,
		item.TaxPercent, item.TaxAmount, item.TaxAmount,
		item.DiscountPercent, item.DiscountAmount, item.DiscountAmount,
		item.StoreID, productOptionsArg,
	)
	if err != nil {
		return 0, fmt.Errorf("insert order item %s: %w", item.SKU, err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func generateProtectCode() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
