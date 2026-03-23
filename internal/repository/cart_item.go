package repository

import (
	"context"
	"database/sql"
	"fmt"
)

// CartItemData holds a quote_item row.
type CartItemData struct {
	ItemID        int
	QuoteID       int
	ProductID     int
	ProductType   string
	SKU           string
	Name          string
	Qty           float64
	Price         float64
	BasePrice     float64
	RowTotal      float64
	BaseRowTotal  float64
	TaxPercent    float64
	TaxAmount     float64
	DiscountAmount     float64
	ParentItemID       *int
	ProductTaxClassID  int // resolved from catalog_product_entity_int
}

type CartItemRepository struct {
	db *sql.DB
}

func NewCartItemRepository(db *sql.DB) *CartItemRepository {
	return &CartItemRepository{db: db}
}

// GetByQuoteID loads all items for a cart.
func (r *CartItemRepository) GetByQuoteID(ctx context.Context, quoteID int) ([]*CartItemData, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT item_id, quote_id, product_id, product_type, sku, name,
		       qty, COALESCE(price, 0), COALESCE(base_price, 0),
		       COALESCE(row_total, 0), COALESCE(base_row_total, 0),
		       COALESCE(tax_percent, 0), COALESCE(tax_amount, 0),
		       COALESCE(discount_amount, 0), parent_item_id
		FROM quote_item
		WHERE quote_id = ?
		ORDER BY item_id`, quoteID)
	if err != nil {
		return nil, fmt.Errorf("load cart items: %w", err)
	}
	defer rows.Close()

	var items []*CartItemData
	for rows.Next() {
		var item CartItemData
		if err := rows.Scan(
			&item.ItemID, &item.QuoteID, &item.ProductID, &item.ProductType,
			&item.SKU, &item.Name, &item.Qty, &item.Price, &item.BasePrice,
			&item.RowTotal, &item.BaseRowTotal, &item.TaxPercent, &item.TaxAmount,
			&item.DiscountAmount, &item.ParentItemID,
		); err != nil {
			return nil, fmt.Errorf("scan cart item: %w", err)
		}
		items = append(items, &item)
	}
	return items, rows.Err()
}

// Add inserts a new item or increments quantity if same product already in cart.
func (r *CartItemRepository) Add(ctx context.Context, quoteID, productID int, sku, name, productType string, qty, price float64) (int, error) {
	// Check for existing item with same product
	var existingID int
	var existingQty float64
	err := r.db.QueryRowContext(ctx,
		"SELECT item_id, qty FROM quote_item WHERE quote_id = ? AND product_id = ? AND parent_item_id IS NULL",
		quoteID, productID,
	).Scan(&existingID, &existingQty)

	if err == nil {
		// Existing item — increment qty
		newQty := existingQty + qty
		rowTotal := price * newQty
		_, err := r.db.ExecContext(ctx,
			"UPDATE quote_item SET qty = ?, row_total = ?, base_row_total = ?, updated_at = NOW() WHERE item_id = ?",
			newQty, rowTotal, rowTotal, existingID,
		)
		if err != nil {
			return 0, fmt.Errorf("update cart item qty: %w", err)
		}
		return existingID, nil
	}

	// New item
	rowTotal := price * qty
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO quote_item (quote_id, product_id, product_type, sku, name,
			qty, price, base_price, row_total, base_row_total,
			store_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			(SELECT store_id FROM quote WHERE entity_id = ?),
			NOW(), NOW())`,
		quoteID, productID, productType, sku, name,
		qty, price, price, rowTotal, rowTotal,
		quoteID,
	)
	if err != nil {
		return 0, fmt.Errorf("add cart item: %w", err)
	}
	id, _ := result.LastInsertId()
	return int(id), nil
}

// UpdateQty changes the quantity of a cart item.
func (r *CartItemRepository) UpdateQty(ctx context.Context, itemID int, qty float64) error {
	// Get the item's price to recalculate row_total
	var price float64
	if err := r.db.QueryRowContext(ctx, "SELECT COALESCE(price, 0) FROM quote_item WHERE item_id = ?", itemID).Scan(&price); err != nil {
		return fmt.Errorf("item %d not found: %w", itemID, err)
	}
	rowTotal := price * qty
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote_item SET qty = ?, row_total = ?, base_row_total = ?, updated_at = NOW() WHERE item_id = ?",
		qty, rowTotal, rowTotal, itemID,
	)
	return err
}

// Remove deletes a cart item.
func (r *CartItemRepository) Remove(ctx context.Context, itemID int) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM quote_item WHERE item_id = ?", itemID)
	return err
}

// GetItemQuoteID returns the quote_id for a given item_id (for auth validation).
func (r *CartItemRepository) GetItemQuoteID(ctx context.Context, itemID int) (int, error) {
	var quoteID int
	err := r.db.QueryRowContext(ctx, "SELECT quote_id FROM quote_item WHERE item_id = ?", itemID).Scan(&quoteID)
	return quoteID, err
}
