// Package errors provides centralized, Magento-compatible error messages.
// All user-facing error strings must match Magento PHP exactly.
package errors

import "fmt"

// Cart errors
var (
	ErrCartNotFound = func(id string) error {
		return fmt.Errorf("Could not find a cart with ID \"%s\"", id)
	}
	ErrCartNotActive = fmt.Errorf("The cart isn't active.")
	ErrCartForbidden = func(id string) error {
		return fmt.Errorf("The current user cannot perform operations on cart \"%s\"", id)
	}
)

// Auth errors
var ErrUnauthorized = fmt.Errorf("The current customer isn't authorized.")

// Product errors
var (
	ErrProductNotFound = func(sku string) error {
		return fmt.Errorf("Could not find a product with SKU \"%s\"", sku)
	}
	ErrNotSalable = func(sku string) error {
		return fmt.Errorf("Product \"%s\" is not available for purchase.", sku)
	}
	ErrOutOfStock = func(sku string) error {
		return fmt.Errorf("Product \"%s\" is out of stock.", sku)
	}
)

// Shipping errors
var (
	ErrCarrierNotFound = func(code string) error {
		return fmt.Errorf("Carrier with such method not found: %s", code)
	}
	ErrShippingMethodMissing = fmt.Errorf("The shipping method is missing. Select the shipping method and try again.")
)

// Address errors
var ErrAddressInvalid = fmt.Errorf("Some addresses can't be used due to the configurations for specific countries.")

// Payment errors
var (
	ErrPaymentNotAvailable = fmt.Errorf("The requested Payment Method is not available.")
	ErrPaymentMissing      = fmt.Errorf("Enter a valid payment method and try again.")
)

// Guest errors
var ErrGuestEmailMissing = fmt.Errorf("Guest email for cart is missing.")

// Order errors
var ErrPlaceOrderFailed = fmt.Errorf("Unable to place order: A server error stopped your order from being placed. Please try to place your order again.")
