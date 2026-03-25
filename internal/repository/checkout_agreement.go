package repository

import (
	"context"
	"database/sql"
	"fmt"
)

// CheckoutAgreementData holds a checkout_agreement row.
type CheckoutAgreementData struct {
	AgreementID   int
	Name          string
	Content       string
	ContentHeight *string
	CheckboxText  string
	IsHTML        bool
	Mode          int // 0=AUTO, 1=MANUAL
}

type CheckoutAgreementRepository struct {
	db *sql.DB
}

func NewCheckoutAgreementRepository(db *sql.DB) *CheckoutAgreementRepository {
	return &CheckoutAgreementRepository{db: db}
}

// GetActiveByStore returns all active checkout agreements for the given store.
func (r *CheckoutAgreementRepository) GetActiveByStore(ctx context.Context, storeID int) ([]*CheckoutAgreementData, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.agreement_id, COALESCE(a.name,''), COALESCE(a.content,''),
		       a.content_height, COALESCE(a.checkbox_text,''),
		       a.is_html, a.mode
		FROM checkout_agreement a
		INNER JOIN checkout_agreement_store s ON a.agreement_id = s.agreement_id
		WHERE a.is_active = 1 AND s.store_id = ?
		ORDER BY a.agreement_id`,
		storeID,
	)
	if err != nil {
		return nil, fmt.Errorf("load checkout agreements: %w", err)
	}
	defer rows.Close()

	var result []*CheckoutAgreementData
	for rows.Next() {
		var a CheckoutAgreementData
		if err := rows.Scan(
			&a.AgreementID, &a.Name, &a.Content,
			&a.ContentHeight, &a.CheckboxText,
			&a.IsHTML, &a.Mode,
		); err != nil {
			return nil, fmt.Errorf("scan checkout agreement: %w", err)
		}
		result = append(result, &a)
	}
	return result, rows.Err()
}
