// Package payment provides Stripe Checkout integration.
// The service uses Stripe Checkout (hosted payment page) — no card details
// are ever handled server-side. Flow:
//   1. createStripeCheckoutSession mutation → StripeClient.CreateCheckoutSession
//   2. storefront redirects to checkout_url (Stripe-hosted)
//   3. Stripe fires checkout.session.completed webhook
//   4. webhook handler verifies signature, places order, stores payment_intent_id
package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/refund"
	"github.com/stripe/stripe-go/v81/webhook"
)

// StripeClient wraps the Stripe API.
type StripeClient struct {
	secretKey     string
	webhookSecret string
}

// LineItem is a simplified cart item for Stripe line items.
type LineItem struct {
	Name     string
	Amount   int64  // in smallest currency unit (cents)
	Currency string // e.g. "usd"
	Qty      int64
}

// CheckoutSessionResult holds the redirect URL and session ID.
type CheckoutSessionResult struct {
	URL       string
	SessionID string
}

// NewStripeClient creates a StripeClient. Returns nil if secretKey is empty.
func NewStripeClient(secretKey, webhookSecret string) *StripeClient {
	if secretKey == "" {
		return nil
	}
	return &StripeClient{secretKey: secretKey, webhookSecret: webhookSecret}
}

// CreateCheckoutSession creates a Stripe Checkout Session.
// cartID is stored in metadata so the webhook can retrieve it.
func (c *StripeClient) CreateCheckoutSession(ctx context.Context, cartID string, items []LineItem, successURL, cancelURL string) (*CheckoutSessionResult, error) {
	stripe.Key = c.secretKey

	lineItems := make([]*stripe.CheckoutSessionLineItemParams, 0, len(items))
	for _, item := range items {
		lineItems = append(lineItems, &stripe.CheckoutSessionLineItemParams{
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency: stripe.String(item.Currency),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String(item.Name),
				},
				UnitAmount: stripe.Int64(item.Amount),
			},
			Quantity: stripe.Int64(item.Qty),
		})
	}

	params := &stripe.CheckoutSessionParams{
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		LineItems:          lineItems,
		Mode:               stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL:         stripe.String(successURL),
		CancelURL:          stripe.String(cancelURL),
		Metadata: map[string]string{
			"cart_id": cartID,
		},
	}

	sess, err := session.New(params)
	if err != nil {
		return nil, fmt.Errorf("stripe: create session: %w", err)
	}

	log.Info().Str("session_id", sess.ID).Str("cart_id", cartID).Msg("stripe checkout session created")
	return &CheckoutSessionResult{URL: sess.URL, SessionID: sess.ID}, nil
}

// ParseWebhookEvent reads and verifies a Stripe webhook request.
// Returns the parsed Event or an error if the signature is invalid.
func (c *StripeClient) ParseWebhookEvent(r *http.Request) (*stripe.Event, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("stripe webhook: read body: %w", err)
	}

	sig := r.Header.Get("Stripe-Signature")
	if c.webhookSecret == "" {
		// Dev mode: skip signature verification, parse event directly
		log.Warn().Msg("stripe webhook: no webhook secret configured, skipping signature verification")
		event, err := webhook.ConstructEventIgnoringTolerance(body, sig, "dev_skip")
		if err != nil {
			// If that fails too, just try unmarshalling
			var ev stripe.Event
			if jsonErr := json.Unmarshal(body, &ev); jsonErr != nil {
				return nil, fmt.Errorf("stripe webhook: unmarshal: %w", jsonErr)
			}
			return &ev, nil
		}
		return &event, nil
	}

	event, err := webhook.ConstructEvent(body, sig, c.webhookSecret)
	if err != nil {
		return nil, fmt.Errorf("stripe webhook: invalid signature: %w", err)
	}
	return &event, nil
}

// CreateRefund issues a full or partial refund for a PaymentIntent.
// amountCents = 0 means full refund.
func (c *StripeClient) CreateRefund(ctx context.Context, paymentIntentID string, amountCents int64) (string, error) {
	stripe.Key = c.secretKey

	params := &stripe.RefundParams{
		PaymentIntent: stripe.String(paymentIntentID),
	}
	if amountCents > 0 {
		params.Amount = stripe.Int64(amountCents)
	}

	r, err := refund.New(params)
	if err != nil {
		return "", fmt.Errorf("stripe: create refund: %w", err)
	}
	log.Info().Str("refund_id", r.ID).Str("payment_intent", paymentIntentID).Msg("stripe refund created")
	return r.ID, nil
}

// Configured returns true if this client has a secret key.
func (c *StripeClient) Configured() bool {
	return c != nil && c.secretKey != ""
}
