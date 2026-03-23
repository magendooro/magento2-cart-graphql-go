package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
)

type CartMaskRepository struct {
	db *sql.DB
}

func NewCartMaskRepository(db *sql.DB) *CartMaskRepository {
	return &CartMaskRepository{db: db}
}

// Create generates a masked ID for a cart and stores the mapping.
func (r *CartMaskRepository) Create(ctx context.Context, quoteID int) (string, error) {
	maskedID, err := generateMaskedID()
	if err != nil {
		return "", fmt.Errorf("generate masked ID: %w", err)
	}

	_, err = r.db.ExecContext(ctx,
		"INSERT INTO quote_id_mask (quote_id, masked_id) VALUES (?, ?)",
		quoteID, maskedID,
	)
	if err != nil {
		return "", fmt.Errorf("store cart mask: %w", err)
	}
	return maskedID, nil
}

// Resolve converts a masked ID to a quote entity_id.
func (r *CartMaskRepository) Resolve(ctx context.Context, maskedID string) (int, error) {
	var quoteID int
	err := r.db.QueryRowContext(ctx,
		"SELECT quote_id FROM quote_id_mask WHERE masked_id = ?",
		maskedID,
	).Scan(&quoteID)
	if err != nil {
		return 0, fmt.Errorf("invalid cart ID: %w", err)
	}
	return quoteID, nil
}

// GetMaskedID returns the masked ID for a quote entity_id.
func (r *CartMaskRepository) GetMaskedID(ctx context.Context, quoteID int) (string, error) {
	var maskedID string
	err := r.db.QueryRowContext(ctx,
		"SELECT masked_id FROM quote_id_mask WHERE quote_id = ?",
		quoteID,
	).Scan(&maskedID)
	if err == sql.ErrNoRows {
		// Create one if missing (customer carts may not have masks)
		return r.Create(ctx, quoteID)
	}
	if err != nil {
		return "", err
	}
	return maskedID, nil
}

func generateMaskedID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
