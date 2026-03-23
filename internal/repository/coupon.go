package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// SalesRule holds a cart price rule.
type SalesRule struct {
	RuleID         int
	Name           string
	IsActive       int
	SimpleAction   string  // by_percent, by_fixed, cart_fixed, buy_x_get_y
	DiscountAmount float64
	DiscountQty    *float64
	CouponType     int // 1=no coupon, 2=specific coupon, 3=auto
	FromDate       *time.Time
	ToDate         *time.Time
	StopRulesProcessing int
	SortOrder      int
	UsesPerCoupon  int
	UsesPerCustomer int
	ApplyToShipping int
	Description    *string
}

// SalesRuleCoupon holds a coupon code.
type SalesRuleCoupon struct {
	CouponID   int
	RuleID     int
	Code       string
	UsageLimit *int
	TimesUsed  int
}

type CouponRepository struct {
	db *sql.DB
}

func NewCouponRepository(db *sql.DB) *CouponRepository {
	return &CouponRepository{db: db}
}

// LookupCoupon validates a coupon code and returns the coupon + rule.
// Returns error if the coupon doesn't exist or the rule is invalid.
func (r *CouponRepository) LookupCoupon(ctx context.Context, code string, websiteID, customerGroupID int) (*SalesRuleCoupon, *SalesRule, error) {
	// Find coupon by code
	var coupon SalesRuleCoupon
	err := r.db.QueryRowContext(ctx,
		"SELECT coupon_id, rule_id, code, usage_limit, times_used FROM salesrule_coupon WHERE code = ?",
		code,
	).Scan(&coupon.CouponID, &coupon.RuleID, &coupon.Code, &coupon.UsageLimit, &coupon.TimesUsed)
	if err != nil {
		return nil, nil, fmt.Errorf("coupon not found")
	}

	// Check usage limits
	if coupon.UsageLimit != nil && *coupon.UsageLimit > 0 && coupon.TimesUsed >= *coupon.UsageLimit {
		return nil, nil, fmt.Errorf("coupon usage limit exceeded")
	}

	// Load the rule
	var rule SalesRule
	err = r.db.QueryRowContext(ctx, `
		SELECT rule_id, name, is_active, COALESCE(simple_action, ''), COALESCE(discount_amount, 0),
		       discount_qty, coupon_type, from_date, to_date, stop_rules_processing,
		       sort_order, uses_per_coupon, uses_per_customer, apply_to_shipping, description
		FROM salesrule WHERE rule_id = ?`,
		coupon.RuleID,
	).Scan(&rule.RuleID, &rule.Name, &rule.IsActive, &rule.SimpleAction, &rule.DiscountAmount,
		&rule.DiscountQty, &rule.CouponType, &rule.FromDate, &rule.ToDate, &rule.StopRulesProcessing,
		&rule.SortOrder, &rule.UsesPerCoupon, &rule.UsesPerCustomer, &rule.ApplyToShipping, &rule.Description)
	if err != nil {
		return nil, nil, fmt.Errorf("rule not found")
	}

	// Validate rule
	if rule.IsActive != 1 {
		return nil, nil, fmt.Errorf("rule not active")
	}
	now := time.Now()
	if rule.FromDate != nil && now.Before(*rule.FromDate) {
		return nil, nil, fmt.Errorf("rule not yet active")
	}
	if rule.ToDate != nil && now.After(*rule.ToDate) {
		return nil, nil, fmt.Errorf("rule expired")
	}

	// Check website
	var websiteCount int
	r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM salesrule_website WHERE rule_id = ? AND website_id = ?",
		rule.RuleID, websiteID,
	).Scan(&websiteCount)
	if websiteCount == 0 {
		return nil, nil, fmt.Errorf("rule not available for website")
	}

	// Check customer group
	var groupCount int
	r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM salesrule_customer_group WHERE rule_id = ? AND customer_group_id = ?",
		rule.RuleID, customerGroupID,
	).Scan(&groupCount)
	if groupCount == 0 {
		return nil, nil, fmt.Errorf("rule not available for customer group")
	}

	return &coupon, &rule, nil
}

// GetRuleActionSkus returns the SKUs that a rule's action conditions target.
// Returns nil if the rule applies to all products.
func (r *CouponRepository) GetRuleActionSkus(ctx context.Context, ruleID int) []string {
	var actionsJSON string
	err := r.db.QueryRowContext(ctx,
		"SELECT COALESCE(actions_serialized, '') FROM salesrule WHERE rule_id = ?",
		ruleID,
	).Scan(&actionsJSON)
	if err != nil || actionsJSON == "" {
		return nil
	}

	// Simple extraction: look for "attribute":"sku" conditions
	// Full condition parsing would require a rule engine; for now extract SKU values
	// Format: {"conditions":[{"attribute":"sku","operator":"==","value":"24-UG06"}]}
	skus := extractSkusFromConditions(actionsJSON)
	return skus
}

// extractSkusFromConditions does a simple extraction of SKU values from Magento's
// serialized rule conditions. This handles the common case of sku == value conditions.
func extractSkusFromConditions(json string) []string {
	var skus []string
	// Look for patterns like "attribute":"sku"..."value":"SKU-VALUE"
	parts := strings.Split(json, `"attribute":"sku"`)
	for i := 1; i < len(parts); i++ {
		// Find the value field after this sku attribute
		valueIdx := strings.Index(parts[i], `"value":"`)
		if valueIdx == -1 {
			continue
		}
		valueStart := valueIdx + len(`"value":"`)
		valueEnd := strings.Index(parts[i][valueStart:], `"`)
		if valueEnd == -1 {
			continue
		}
		sku := parts[i][valueStart : valueStart+valueEnd]
		if sku != "" {
			skus = append(skus, sku)
		}
	}
	return skus
}

// SetCouponOnQuote stores the coupon code and applied rule IDs on the quote.
func (r *CouponRepository) SetCouponOnQuote(ctx context.Context, quoteID int, couponCode string, appliedRuleIDs string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote SET coupon_code = ?, applied_rule_ids = ?, updated_at = NOW() WHERE entity_id = ?",
		couponCode, appliedRuleIDs, quoteID,
	)
	return err
}

// ClearCouponOnQuote removes the coupon from the quote.
func (r *CouponRepository) ClearCouponOnQuote(ctx context.Context, quoteID int) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote SET coupon_code = NULL, applied_rule_ids = NULL, updated_at = NOW() WHERE entity_id = ?",
		quoteID,
	)
	return err
}

// UpdateItemDiscount sets the discount on a quote item.
func (r *CouponRepository) UpdateItemDiscount(ctx context.Context, itemID int, discountAmount, discountPercent float64, appliedRuleIDs string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote_item SET discount_amount = ?, discount_percent = ?, applied_rule_ids = ?, updated_at = NOW() WHERE item_id = ?",
		discountAmount, discountPercent, appliedRuleIDs, itemID,
	)
	return err
}

// ClearItemDiscounts resets discount on all items for a quote.
func (r *CouponRepository) ClearItemDiscounts(ctx context.Context, quoteID int) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote_item SET discount_amount = 0, discount_percent = 0, applied_rule_ids = NULL, updated_at = NOW() WHERE quote_id = ?",
		quoteID,
	)
	return err
}
