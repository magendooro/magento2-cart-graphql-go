package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CartData holds a quote row.
type CartData struct {
	EntityID             int
	StoreID              int
	IsActive             int
	IsVirtual            int
	ItemsCount           int
	ItemsQty             float64
	GrandTotal           float64
	BaseGrandTotal       float64
	Subtotal             float64
	SubtotalWithDiscount float64
	TaxAmount            float64
	ShippingAmount       float64
	CouponCode           *string
	CustomerID           *int
	CustomerEmail        *string
	CustomerIsGuest      int
	CustomerGroupID      int
	BaseCurrencyCode     string
	QuoteCurrencyCode    string
	ReservedOrderID      *string
	UpdatedAt            time.Time
}

type CartRepository struct {
	db *sql.DB
}

func NewCartRepository(db *sql.DB) *CartRepository {
	return &CartRepository{db: db}
}

// Create inserts a new empty quote and returns its entity_id.
func (r *CartRepository) Create(ctx context.Context, storeID int, customerID *int) (int, error) {
	isGuest := 1
	customerGroupID := 0
	if customerID != nil {
		isGuest = 0
		customerGroupID = r.lookupCustomerGroupID(ctx, *customerID)
	}

	result, err := r.db.ExecContext(ctx, `
		INSERT INTO quote (store_id, is_active, items_count, items_qty, grand_total, base_grand_total,
			subtotal, base_subtotal, customer_id, customer_is_guest, customer_group_id,
			base_currency_code, store_currency_code, quote_currency_code,
			created_at, updated_at)
		VALUES (?, 1, 0, 0, 0, 0, 0, 0, ?, ?, ?, 'USD', 'USD', 'USD', NOW(), NOW())`,
		storeID, customerID, isGuest, customerGroupID,
	)
	if err != nil {
		return 0, fmt.Errorf("create cart: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	return int(id), nil
}

// GetByID loads a cart by entity_id.
func (r *CartRepository) GetByID(ctx context.Context, entityID int) (*CartData, error) {
	var c CartData
	err := r.db.QueryRowContext(ctx, `
		SELECT entity_id, store_id, is_active, COALESCE(is_virtual, 0),
		       items_count, COALESCE(items_qty, 0),
		       COALESCE(grand_total, 0), COALESCE(base_grand_total, 0),
		       COALESCE(subtotal, 0), COALESCE(subtotal_with_discount, 0),
		       coupon_code, customer_id, customer_email, COALESCE(customer_is_guest, 1),
		       COALESCE(customer_group_id, 0),
		       COALESCE(base_currency_code, 'USD'), COALESCE(quote_currency_code, 'USD'),
		       reserved_order_id, updated_at
		FROM quote WHERE entity_id = ?`, entityID,
	).Scan(
		&c.EntityID, &c.StoreID, &c.IsActive, &c.IsVirtual,
		&c.ItemsCount, &c.ItemsQty,
		&c.GrandTotal, &c.BaseGrandTotal,
		&c.Subtotal, &c.SubtotalWithDiscount,
		&c.CouponCode, &c.CustomerID, &c.CustomerEmail, &c.CustomerIsGuest,
		&c.CustomerGroupID,
		&c.BaseCurrencyCode, &c.QuoteCurrencyCode,
		&c.ReservedOrderID, &c.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("cart %d not found: %w", entityID, err)
	}
	return &c, nil
}

// GetActiveByCustomerID finds the active cart for a customer.
func (r *CartRepository) GetActiveByCustomerID(ctx context.Context, customerID, storeID int) (*CartData, error) {
	var entityID int
	err := r.db.QueryRowContext(ctx,
		"SELECT entity_id FROM quote WHERE customer_id = ? AND store_id = ? AND is_active = 1 ORDER BY entity_id DESC LIMIT 1",
		customerID, storeID,
	).Scan(&entityID)
	if err != nil {
		return nil, fmt.Errorf("no active cart for customer %d: %w", customerID, err)
	}
	return r.GetByID(ctx, entityID)
}

// UpdateTotals updates the computed totals on the quote.
// Must be called within a transaction obtained via BeginTotalsUpdate/GetByIDForUpdate
// to ensure concurrent safety (the FOR UPDATE lock prevents double-writes).
func (r *CartRepository) UpdateTotals(ctx context.Context, entityID int, subtotal, grandTotal, discountAmount float64, itemsCount int, itemsQty float64, isVirtual bool, expectedUpdatedAt time.Time) error {
	subtotalWithDiscount := subtotal - discountAmount
	virtualFlag := 0
	if isVirtual {
		virtualFlag = 1
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE quote SET subtotal = ?, base_subtotal = ?,
			grand_total = ?, base_grand_total = ?,
			subtotal_with_discount = ?,
			items_count = ?, items_qty = ?, is_virtual = ?,
			updated_at = NOW()
		WHERE entity_id = ?`,
		subtotal, subtotal,
		grandTotal, grandTotal,
		subtotalWithDiscount,
		itemsCount, itemsQty, virtualFlag, entityID,
	)
	return err
}

// BeginTotalsUpdate starts a transaction and locks the quote row with SELECT FOR UPDATE.
// Returns the locked CartData and the transaction. The caller must Commit or Rollback.
// Returns ErrCartConflict if the lock cannot be acquired immediately (unlikely with InnoDB).
func (r *CartRepository) BeginTotalsUpdate(ctx context.Context, entityID int) (*sql.Tx, *CartData, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	var c CartData
	err = tx.QueryRowContext(ctx, `
		SELECT entity_id, store_id, is_active, COALESCE(is_virtual, 0),
		       items_count, COALESCE(items_qty, 0),
		       COALESCE(grand_total, 0), COALESCE(base_grand_total, 0),
		       COALESCE(subtotal, 0), COALESCE(subtotal_with_discount, 0),
		       coupon_code, customer_id, customer_email, COALESCE(customer_is_guest, 1),
		       COALESCE(customer_group_id, 0),
		       COALESCE(base_currency_code, 'USD'), COALESCE(quote_currency_code, 'USD'),
		       reserved_order_id, updated_at
		FROM quote WHERE entity_id = ? FOR UPDATE`, entityID,
	).Scan(
		&c.EntityID, &c.StoreID, &c.IsActive, &c.IsVirtual,
		&c.ItemsCount, &c.ItemsQty,
		&c.GrandTotal, &c.BaseGrandTotal,
		&c.Subtotal, &c.SubtotalWithDiscount,
		&c.CouponCode, &c.CustomerID, &c.CustomerEmail, &c.CustomerIsGuest,
		&c.CustomerGroupID,
		&c.BaseCurrencyCode, &c.QuoteCurrencyCode,
		&c.ReservedOrderID, &c.UpdatedAt,
	)
	if err != nil {
		tx.Rollback()
		return nil, nil, fmt.Errorf("cart %d not found: %w", entityID, err)
	}
	return tx, &c, nil
}

// UpdateTotalsTx runs UpdateTotals within an existing transaction.
func (r *CartRepository) UpdateTotalsTx(ctx context.Context, tx *sql.Tx, entityID int, subtotal, grandTotal, discountAmount float64, itemsCount int, itemsQty float64, isVirtual bool) error {
	subtotalWithDiscount := subtotal - discountAmount
	virtualFlag := 0
	if isVirtual {
		virtualFlag = 1
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE quote SET subtotal = ?, base_subtotal = ?,
			grand_total = ?, base_grand_total = ?,
			subtotal_with_discount = ?,
			items_count = ?, items_qty = ?, is_virtual = ?,
			updated_at = NOW()
		WHERE entity_id = ?`,
		subtotal, subtotal,
		grandTotal, grandTotal,
		subtotalWithDiscount,
		itemsCount, itemsQty, virtualFlag, entityID,
	)
	return err
}

// UpdateEmail sets the guest email on the cart.
func (r *CartRepository) UpdateEmail(ctx context.Context, entityID int, email string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote SET customer_email = ?, updated_at = NOW() WHERE entity_id = ?",
		email, entityID,
	)
	return err
}

// Deactivate marks a cart as inactive (after order placement).
func (r *CartRepository) Deactivate(ctx context.Context, entityID int, reservedOrderID string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote SET is_active = 0, reserved_order_id = ?, updated_at = NOW() WHERE entity_id = ?",
		reservedOrderID, entityID,
	)
	return err
}

// SetCustomer assigns a customer to a cart (for assignCustomerToGuestCart).
func (r *CartRepository) SetCustomer(ctx context.Context, entityID, customerID int) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote SET customer_id = ?, customer_is_guest = 0, updated_at = NOW() WHERE entity_id = ?",
		customerID, entityID,
	)
	return err
}

// DeactivateSimple marks a cart as inactive without setting reserved_order_id.
func (r *CartRepository) DeactivateSimple(ctx context.Context, entityID int) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote SET is_active = 0, updated_at = NOW() WHERE entity_id = ?",
		entityID,
	)
	return err
}

// DB returns the underlying database connection (for product lookups).
func (r *CartRepository) DB() *sql.DB {
	return r.db
}

// lookupCustomerGroupID returns the customer_group_id from customer_entity.
// Returns 0 (NOT LOGGED IN) if the customer is not found.
func (r *CartRepository) lookupCustomerGroupID(ctx context.Context, customerID int) int {
	var groupID int
	r.db.QueryRowContext(ctx,
		"SELECT COALESCE(group_id, 0) FROM customer_entity WHERE entity_id = ?",
		customerID,
	).Scan(&groupID)
	return groupID
}
