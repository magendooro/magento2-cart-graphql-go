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
var (
	ErrAddressInvalid      = fmt.Errorf("Some addresses can't be used due to the configurations for specific countries.")
	ErrBillingAddressMissing = fmt.Errorf("Please check the billing address information.")
)

// Payment errors
var (
	ErrPaymentNotAvailable = fmt.Errorf("The requested Payment Method is not available.")
	ErrPaymentMissing      = fmt.Errorf("Enter a valid payment method and try again.")
)

// Guest errors
var (
	ErrGuestEmailMissing        = fmt.Errorf("Guest email for cart is missing.")
	ErrGuestEmailInvalid        = fmt.Errorf("Invalid email format")
	ErrGuestEmailNotAllowed     = fmt.Errorf("The request is not allowed for logged in customers")
	ErrGuestCheckoutNotAllowed  = fmt.Errorf("Guest checkout is not allowed. Register a customer account or login with existing one.")
)

// Multiple address / method errors
var (
	ErrMultipleShippingAddresses = fmt.Errorf("You cannot specify multiple shipping addresses.")
	ErrMultipleShippingMethods   = fmt.Errorf("You cannot specify multiple shipping methods.")
)

// Billing address input errors (Magento SetBillingAddressOnCart)
var (
	ErrBillingAddressInputMissing    = fmt.Errorf(`The billing address must contain either "customer_address_id", "address", or "same_as_shipping".`)
	ErrBillingAddressInputConflict   = fmt.Errorf(`The billing address cannot contain "customer_address_id" and "address" at the same time.`)
	ErrSameAsShippingNotSet          = fmt.Errorf(`Could not use the "same_as_shipping" option, because the shipping address has not been set.`)
	ErrSameAsShippingMultipleAddrs   = fmt.Errorf(`Could not use the "same_as_shipping" option, because multiple shipping addresses have been set.`)
)

// Order errors
var (
	ErrPlaceOrderFailed  = fmt.Errorf("Unable to place order: A server error stopped your order from being placed. Please try to place your order again.")
	ErrCartConflict      = fmt.Errorf("Cart was modified by another request, please retry.")
	ErrInsufficientStock = func(sku string, requested, available float64) error {
		return fmt.Errorf("Not enough items for \"%s\" in stock. Requested: %.0f, Available: %.0f.", sku, requested, available)
	}
	ErrOrderNotFound = func(incrementID string) error {
		return fmt.Errorf("No such entity with incrementId = %s", incrementID)
	}
	ErrOrderStatusInvalid = func(from, to string) error {
		return fmt.Errorf("Cannot change order status from \"%s\" to \"%s\".", from, to)
	}
)
