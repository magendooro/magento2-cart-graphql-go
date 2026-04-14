package service

import (
	"github.com/magendooro/magento2-cart-graphql-go/graph/model"
	carterr "github.com/magendooro/magento2-cart-graphql-go/internal/errors"
	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
)

// validateForOrder checks all pre-conditions for PlaceOrder and returns a
// PlaceOrderOutput with errors if any condition fails, or nil if the cart is ready.
// requestCustomerID is the customer ID from the JWT in the request context (0 = guest).
func validateForOrder(
	cart *repository.CartData,
	items []*repository.CartItemData,
	addrs []*repository.CartAddressData,
	payment *repository.PaymentMethod,
	requestCustomerID int,
) *model.PlaceOrderOutput {
	if len(items) == 0 {
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrPlaceOrderFailed.Error())
	}

	var hasShipping, hasBilling bool
	for _, a := range addrs {
		switch a.AddressType {
		case "shipping":
			hasShipping = true
			if a.ShippingMethod == nil || *a.ShippingMethod == "" {
				return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrShippingMethodMissing.Error())
			}
		case "billing":
			hasBilling = true
		}
	}

	if cart.IsVirtual != 1 && !hasShipping {
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrAddressInvalid.Error())
	}
	if !hasBilling {
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrBillingAddressMissing.Error())
	}

	if payment == nil || payment.Code == "" {
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrPaymentMissing.Error())
	}

	if cart.CustomerID == nil || *cart.CustomerID == 0 {
		// Skip email check when an authenticated customer is placing the order
		// (their identity is established via JWT even if the cart has no CustomerID).
		if requestCustomerID == 0 && (cart.CustomerEmail == nil || *cart.CustomerEmail == "") {
			return orderErr(model.PlaceOrderErrorCodesGuestEmailMissing, carterr.ErrGuestEmailMissing.Error())
		}
	}

	return nil
}

func orderErr(code model.PlaceOrderErrorCodes, msg string) *model.PlaceOrderOutput {
	return &model.PlaceOrderOutput{
		Errors: []*model.PlaceOrderError{{Code: code, Message: msg}},
	}
}
