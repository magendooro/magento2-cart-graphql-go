package graph

import (
	"context"
	"fmt"

	"github.com/magendooro/magento2-cart-graphql-go/graph/model"
)

// CreateEmptyCart is the resolver for the createEmptyCart field.
func (r *mutationResolver) CreateEmptyCart(ctx context.Context, input *model.CreateEmptyCartInput) (*string, error) {
	maskedID, err := r.CartService.CreateEmptyCart(ctx)
	if err != nil {
		return nil, err
	}
	return &maskedID, nil
}

// CreateGuestCart is the resolver for the createGuestCart field.
func (r *mutationResolver) CreateGuestCart(ctx context.Context, input *model.CreateGuestCartInput) (*model.CreateGuestCartOutput, error) {
	maskedID, err := r.CartService.CreateEmptyCart(ctx)
	if err != nil {
		return nil, err
	}
	cart, err := r.CartService.GetCart(ctx, maskedID)
	if err != nil {
		return nil, err
	}
	return &model.CreateGuestCartOutput{Cart: cart}, nil
}

// AddProductsToCart is the resolver for the addProductsToCart field.
func (r *mutationResolver) AddProductsToCart(ctx context.Context, cartID string, cartItems []*model.CartItemInput) (*model.AddProductsToCartOutput, error) {
	return r.CartService.AddProducts(ctx, cartID, cartItems)
}

// UpdateCartItems is the resolver for the updateCartItems field.
func (r *mutationResolver) UpdateCartItems(ctx context.Context, input *model.UpdateCartItemsInput) (*model.UpdateCartItemsOutput, error) {
	cart, err := r.CartService.UpdateItems(ctx, input.CartID, input.CartItems)
	if err != nil {
		return nil, err
	}
	return &model.UpdateCartItemsOutput{Cart: cart}, nil
}

// RemoveItemFromCart is the resolver for the removeItemFromCart field.
func (r *mutationResolver) RemoveItemFromCart(ctx context.Context, input *model.RemoveItemFromCartInput) (*model.RemoveItemFromCartOutput, error) {
	cart, err := r.CartService.RemoveItem(ctx, input.CartID, input.CartItemUID)
	if err != nil {
		return nil, err
	}
	return &model.RemoveItemFromCartOutput{Cart: cart}, nil
}

// SetShippingAddressesOnCart is the resolver for the setShippingAddressesOnCart field.
func (r *mutationResolver) SetShippingAddressesOnCart(ctx context.Context, input *model.SetShippingAddressesOnCartInput) (*model.SetShippingAddressesOnCartOutput, error) {
	cart, err := r.CartService.SetShippingAddresses(ctx, input.CartID, input.ShippingAddresses)
	if err != nil {
		return nil, err
	}
	return &model.SetShippingAddressesOnCartOutput{Cart: cart}, nil
}

// SetBillingAddressOnCart is the resolver for the setBillingAddressOnCart field.
func (r *mutationResolver) SetBillingAddressOnCart(ctx context.Context, input *model.SetBillingAddressOnCartInput) (*model.SetBillingAddressOnCartOutput, error) {
	cart, err := r.CartService.SetBillingAddress(ctx, input.CartID, input.BillingAddress)
	if err != nil {
		return nil, err
	}
	return &model.SetBillingAddressOnCartOutput{Cart: cart}, nil
}

// SetShippingMethodsOnCart is the resolver for the setShippingMethodsOnCart field.
func (r *mutationResolver) SetShippingMethodsOnCart(ctx context.Context, input *model.SetShippingMethodsOnCartInput) (*model.SetShippingMethodsOnCartOutput, error) {
	return nil, fmt.Errorf("not implemented: SetShippingMethodsOnCart")
}

// SetPaymentMethodOnCart is the resolver for the setPaymentMethodOnCart field.
func (r *mutationResolver) SetPaymentMethodOnCart(ctx context.Context, input *model.SetPaymentMethodOnCartInput) (*model.SetPaymentMethodOnCartOutput, error) {
	return nil, fmt.Errorf("not implemented: SetPaymentMethodOnCart")
}

// SetGuestEmailOnCart is the resolver for the setGuestEmailOnCart field.
func (r *mutationResolver) SetGuestEmailOnCart(ctx context.Context, input *model.SetGuestEmailOnCartInput) (*model.SetGuestEmailOnCartOutput, error) {
	cart, err := r.CartService.SetGuestEmail(ctx, input.CartID, input.Email)
	if err != nil {
		return nil, err
	}
	return &model.SetGuestEmailOnCartOutput{Cart: cart}, nil
}

// PlaceOrder is the resolver for the placeOrder field.
func (r *mutationResolver) PlaceOrder(ctx context.Context, input *model.PlaceOrderInput) (*model.PlaceOrderOutput, error) {
	return nil, fmt.Errorf("not implemented: PlaceOrder")
}

// ApplyCouponToCart is the resolver for the applyCouponToCart field.
func (r *mutationResolver) ApplyCouponToCart(ctx context.Context, input *model.ApplyCouponToCartInput) (*model.ApplyCouponToCartOutput, error) {
	return nil, fmt.Errorf("not implemented: ApplyCouponToCart")
}

// RemoveCouponFromCart is the resolver for the removeCouponFromCart field.
func (r *mutationResolver) RemoveCouponFromCart(ctx context.Context, input *model.RemoveCouponFromCartInput) (*model.RemoveCouponFromCartOutput, error) {
	return nil, fmt.Errorf("not implemented: RemoveCouponFromCart")
}

// Cart is the resolver for the cart field.
func (r *queryResolver) Cart(ctx context.Context, cartID string) (*model.Cart, error) {
	return r.CartService.GetCart(ctx, cartID)
}

// CustomerCart is the resolver for the customerCart field.
func (r *queryResolver) CustomerCart(ctx context.Context) (*model.Cart, error) {
	return r.CartService.GetCustomerCart(ctx)
}

// Mutation returns MutationResolver implementation.
func (r *Resolver) Mutation() MutationResolver { return &mutationResolver{r} }

// Query returns QueryResolver implementation.
func (r *Resolver) Query() QueryResolver { return &queryResolver{r} }

type mutationResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
