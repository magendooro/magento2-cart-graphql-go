package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/magendooro/magento2-cart-graphql-go/graph/model"
	"github.com/magendooro/magento2-cart-graphql-go/internal/config"
	carterr "github.com/magendooro/magento2-cart-graphql-go/internal/errors"
	"github.com/magendooro/magento2-cart-graphql-go/internal/middleware"
	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
	"github.com/magendooro/magento2-cart-graphql-go/internal/shipping"
	"github.com/magendooro/magento2-cart-graphql-go/internal/totals"
)

type CartService struct {
	cartRepo         *repository.CartRepository
	maskRepo         *repository.CartMaskRepository
	itemRepo         *repository.CartItemRepository
	addressRepo      *repository.CartAddressRepository
	shippingRepo     *repository.ShippingRepository
	shippingRegistry *shipping.Registry
	paymentRepo      *repository.PaymentRepository
	taxRepo          *repository.TaxRepository
	orderRepo        *repository.OrderRepository
	couponRepo       *repository.CouponRepository
	pipeline         *totals.Pipeline
	cp               *config.ConfigProvider
}

func NewCartService(
	cartRepo *repository.CartRepository,
	maskRepo *repository.CartMaskRepository,
	itemRepo *repository.CartItemRepository,
	addressRepo *repository.CartAddressRepository,
	shippingRepo *repository.ShippingRepository,
	shippingRegistry *shipping.Registry,
	paymentRepo *repository.PaymentRepository,
	taxRepo *repository.TaxRepository,
	orderRepo *repository.OrderRepository,
	couponRepo *repository.CouponRepository,
	cp *config.ConfigProvider,
) *CartService {
	// Build totals pipeline (order matches Magento's sales.xml sort_order)
	pipeline := totals.NewPipeline(
		&totals.SubtotalCollector{},                          // 100
		&totals.DiscountCollector{CouponRepo: couponRepo},    // 300
		&totals.ShippingCollector{},                          // 350
		// &totals.ShippingTaxCollector{},                    // 375 — Phase 3 (#21)
		&totals.TaxCollector{TaxRepo: taxRepo},               // 450
		&totals.GrandTotalCollector{},                        // 550
	)

	return &CartService{
		cartRepo:         cartRepo,
		maskRepo:         maskRepo,
		itemRepo:         itemRepo,
		addressRepo:      addressRepo,
		shippingRepo:     shippingRepo,
		shippingRegistry: shippingRegistry,
		paymentRepo:      paymentRepo,
		taxRepo:          taxRepo,
		orderRepo:        orderRepo,
		couponRepo:       couponRepo,
		pipeline:         pipeline,
		cp:               cp,
	}
}

// CreateEmptyCart creates a new cart and returns its masked ID.
func (s *CartService) CreateEmptyCart(ctx context.Context) (string, error) {
	storeID := middleware.GetStoreID(ctx)
	customerID := middleware.GetCustomerID(ctx)

	var custPtr *int
	if customerID > 0 {
		custPtr = &customerID
	}

	quoteID, err := s.cartRepo.Create(ctx, storeID, custPtr)
	if err != nil {
		return "", err
	}

	maskedID, err := s.maskRepo.Create(ctx, quoteID)
	if err != nil {
		return "", err
	}

	log.Debug().Int("quote_id", quoteID).Str("masked_id", maskedID).Msg("cart created")
	return maskedID, nil
}

// GetCart fetches a cart by masked ID and returns the GraphQL Cart object.
func (s *CartService) GetCart(ctx context.Context, maskedID string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil, err
	}
	if cart.IsActive != 1 {
		return nil, carterr.ErrCartNotActive
	}

	// Auth check: customer carts require matching customer
	customerID := middleware.GetCustomerID(ctx)
	if cart.CustomerID != nil && *cart.CustomerID > 0 && *cart.CustomerID != customerID {
		return nil, carterr.ErrCartForbidden(maskedID)
	}

	return s.mapCart(ctx, cart, maskedID)
}

// GetCustomerCart fetches the active cart for the authenticated customer.
func (s *CartService) GetCustomerCart(ctx context.Context) (*model.Cart, error) {
	customerID := middleware.GetCustomerID(ctx)
	if customerID == 0 {
		return nil, carterr.ErrUnauthorized
	}

	storeID := middleware.GetStoreID(ctx)
	cart, err := s.cartRepo.GetActiveByCustomerID(ctx, customerID, storeID)
	if err != nil {
		// No active cart — create one
		quoteID, err := s.cartRepo.Create(ctx, storeID, &customerID)
		if err != nil {
			return nil, err
		}
		cart, err = s.cartRepo.GetByID(ctx, quoteID)
		if err != nil {
			return nil, err
		}
	}

	maskedID, err := s.maskRepo.GetMaskedID(ctx, cart.EntityID)
	if err != nil {
		return nil, err
	}

	return s.mapCart(ctx, cart, maskedID)
}

// AddProducts adds products to the cart (simple + configurable).
func (s *CartService) AddProducts(ctx context.Context, maskedID string, items []*model.CartItemInput) (*model.AddProductsToCartOutput, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	storeID := middleware.GetStoreID(ctx)
	var userErrors []*model.CartUserInputError

	for _, input := range items {
		// Look up product by SKU
		product, err := s.lookupProduct(ctx, input.Sku, storeID)
		if err != nil {
			userErrors = append(userErrors, &model.CartUserInputError{
				Code:    model.CartUserInputErrorTypeProductNotFound,
				Message: carterr.ErrProductNotFound(input.Sku).Error(),
			})
			continue
		}
		if product.Status != 1 {
			userErrors = append(userErrors, &model.CartUserInputError{
				Code:    model.CartUserInputErrorTypeNotSalable,
				Message: carterr.ErrNotSalable(input.Sku).Error(),
			})
			continue
		}
		if product.StockStatus != 1 {
			userErrors = append(userErrors, &model.CartUserInputError{
				Code:    model.CartUserInputErrorTypeInsufficientStock,
				Message: carterr.ErrOutOfStock(input.Sku).Error(),
			})
			continue
		}

		if product.ProductType == "configurable" && len(input.SelectedOptions) > 0 {
			if err := s.addConfigurableProduct(ctx, quoteID, storeID, product, input); err != nil {
				userErrors = append(userErrors, &model.CartUserInputError{
					Code:    model.CartUserInputErrorTypeUndefined,
					Message: err.Error(),
				})
			}
		} else if product.ProductType == "bundle" && len(input.SelectedOptions) > 0 {
			if err := s.addBundleProduct(ctx, quoteID, storeID, product, input); err != nil {
				userErrors = append(userErrors, &model.CartUserInputError{
					Code:    model.CartUserInputErrorTypeUndefined,
					Message: err.Error(),
				})
			}
		} else {
			_, err = s.itemRepo.Add(ctx, quoteID, product.ProductID, input.Sku, product.Name, product.ProductType, input.Quantity, product.Price)
			if err != nil {
				userErrors = append(userErrors, &model.CartUserInputError{
					Code:    model.CartUserInputErrorTypeUndefined,
					Message: fmt.Sprintf("Could not add \"%s\" to cart: %v", input.Sku, err),
				})
			}
		}
	}

	// Recalculate totals
	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}

	cart, _ := s.GetCart(ctx, maskedID)
	return &model.AddProductsToCartOutput{
		Cart:       cart,
		UserErrors: userErrors,
	}, nil
}

// UpdateItems updates quantities of cart items.
func (s *CartService) UpdateItems(ctx context.Context, maskedID string, items []*model.CartItemUpdateInput) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	for _, item := range items {
		itemID, _ := decodeUID(item.CartItemUID)
		if item.Quantity <= 0 {
			s.itemRepo.Remove(ctx, itemID)
		} else {
			s.itemRepo.UpdateQty(ctx, itemID, item.Quantity)
		}
	}

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}
	return s.GetCart(ctx, maskedID)
}

// RemoveItem removes an item from the cart.
func (s *CartService) RemoveItem(ctx context.Context, maskedID string, itemUID string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}
	_ = quoteID

	itemID, _ := decodeUID(itemUID)
	s.itemRepo.Remove(ctx, itemID)

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}
	return s.GetCart(ctx, maskedID)
}

// SetGuestEmail sets the email on a guest cart.
func (s *CartService) SetGuestEmail(ctx context.Context, maskedID, email string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}
	s.cartRepo.UpdateEmail(ctx, quoteID, email)
	return s.GetCart(ctx, maskedID)
}

// MergeCarts merges a guest cart into the customer's cart.
// Items from source are copied to destination (quantities summed for same SKU).
// Source cart is deactivated after merge.
func (s *CartService) MergeCarts(ctx context.Context, sourceCartID string, destinationCartID *string) (*model.Cart, error) {
	customerID := middleware.GetCustomerID(ctx)
	if customerID == 0 {
		return nil, carterr.ErrUnauthorized
	}

	// Resolve source cart
	sourceQuoteID, err := s.maskRepo.Resolve(ctx, sourceCartID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(sourceCartID)
	}
	sourceCart, err := s.cartRepo.GetByID(ctx, sourceQuoteID)
	if err != nil || sourceCart.IsActive != 1 {
		return nil, carterr.ErrCartNotActive
	}

	// Resolve destination cart (or use customer's active cart)
	var destMaskedID string
	var destQuoteID int
	if destinationCartID != nil && *destinationCartID != "" {
		destMaskedID = *destinationCartID
		destQuoteID, err = s.maskRepo.Resolve(ctx, destMaskedID)
		if err != nil {
			return nil, carterr.ErrCartNotFound(destMaskedID)
		}
	} else {
		// Get customer's active cart
		storeID := middleware.GetStoreID(ctx)
		destCart, err := s.cartRepo.GetActiveByCustomerID(ctx, customerID, storeID)
		if err != nil {
			// Create one if doesn't exist
			newID, err := s.cartRepo.Create(ctx, storeID, &customerID)
			if err != nil {
				return nil, err
			}
			destQuoteID = newID
		} else {
			destQuoteID = destCart.EntityID
		}
		destMaskedID, err = s.maskRepo.GetMaskedID(ctx, destQuoteID)
		if err != nil {
			return nil, err
		}
	}

	// Copy items from source to destination (merge quantities for same product)
	sourceItems, _ := s.itemRepo.GetByQuoteID(ctx, sourceQuoteID)
	for _, item := range sourceItems {
		if item.ParentItemID != nil {
			continue
		}
		if item.ProductType == "configurable" || item.ProductType == "bundle" {
			parentID, _ := s.itemRepo.AddConfigurable(ctx, destQuoteID, item.ProductID, item.SKU, item.Name, item.ProductType, item.Qty, item.Price)
			for _, child := range sourceItems {
				if child.ParentItemID != nil && *child.ParentItemID == item.ItemID {
					s.itemRepo.AddChild(ctx, destQuoteID, child.ProductID, child.SKU, child.Name, "simple", child.Qty, parentID)
				}
			}
		} else {
			s.itemRepo.Add(ctx, destQuoteID, item.ProductID, item.SKU, item.Name, item.ProductType, item.Qty, item.Price)
		}
	}

	// Deactivate source cart
	s.cartRepo.DeactivateSimple(ctx, sourceQuoteID)

	// Recalculate destination totals
	if err := s.recalculateTotals(ctx, destQuoteID); err != nil {
		log.Error().Err(err).Int("quote_id", destQuoteID).Msg("totals recalculation failed after merge")
	}

	return s.GetCart(ctx, destMaskedID)
}

// AssignCustomerToGuestCart transfers a guest cart to an authenticated customer.
// The customer's old active cart is deactivated and its items are merged into the guest cart.
func (s *CartService) AssignCustomerToGuestCart(ctx context.Context, guestCartID string) (*model.Cart, error) {
	customerID := middleware.GetCustomerID(ctx)
	if customerID == 0 {
		return nil, carterr.ErrUnauthorized
	}

	guestQuoteID, err := s.maskRepo.Resolve(ctx, guestCartID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(guestCartID)
	}
	guestCart, err := s.cartRepo.GetByID(ctx, guestQuoteID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(guestCartID)
	}

	// Guest cart must not already belong to a customer
	if guestCart.CustomerID != nil && *guestCart.CustomerID > 0 {
		return nil, carterr.ErrCartForbidden(guestCartID)
	}

	storeID := middleware.GetStoreID(ctx)

	// If customer has an existing active cart, merge its items into the guest cart, then deactivate
	if oldCart, err := s.cartRepo.GetActiveByCustomerID(ctx, customerID, storeID); err == nil {
		oldItems, _ := s.itemRepo.GetByQuoteID(ctx, oldCart.EntityID)
		for _, item := range oldItems {
			if item.ParentItemID != nil {
				continue
			}
			if item.ProductType == "configurable" || item.ProductType == "bundle" {
				parentID, _ := s.itemRepo.AddConfigurable(ctx, guestQuoteID, item.ProductID, item.SKU, item.Name, item.ProductType, item.Qty, item.Price)
				for _, child := range oldItems {
					if child.ParentItemID != nil && *child.ParentItemID == item.ItemID {
						s.itemRepo.AddChild(ctx, guestQuoteID, child.ProductID, child.SKU, child.Name, "simple", child.Qty, parentID)
					}
				}
			} else {
				s.itemRepo.Add(ctx, guestQuoteID, item.ProductID, item.SKU, item.Name, item.ProductType, item.Qty, item.Price)
			}
		}
		s.cartRepo.DeactivateSimple(ctx, oldCart.EntityID)
	}

	// Assign customer to guest cart
	s.cartRepo.SetCustomer(ctx, guestQuoteID, customerID)

	// Recalculate totals
	if err := s.recalculateTotals(ctx, guestQuoteID); err != nil {
		log.Error().Err(err).Int("quote_id", guestQuoteID).Msg("totals recalculation failed after assign")
	}

	maskedID, err := s.maskRepo.GetMaskedID(ctx, guestQuoteID)
	if err != nil {
		return nil, err
	}
	return s.GetCart(ctx, maskedID)
}

// EstimateShippingMethods returns available shipping methods for a temporary address
// without persisting anything to the cart.
func (s *CartService) EstimateShippingMethods(ctx context.Context, input model.EstimateShippingMethodsInput) ([]*model.AvailableShippingMethod, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, input.CartID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(input.CartID)
	}
	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(input.CartID)
	}

	storeID := middleware.GetStoreID(ctx)
	req := &shipping.RateRequest{
		StoreID:   storeID,
		WebsiteID: s.cp.GetWebsiteID(storeID),
		CountryID: input.Address.CountryCode,
		RegionID:  input.Address.RegionID,
		Postcode:  input.Address.Postcode,
		Subtotal:  cart.Subtotal,
		ItemQty:   cart.ItemsQty,
	}
	rates := s.shippingRegistry.CollectRates(ctx, req)

	currency := model.CurrencyEnum(cart.QuoteCurrencyCode)
	var result []*model.AvailableShippingMethod
	for _, r := range rates {
		price := r.Price
		result = append(result, &model.AvailableShippingMethod{
			CarrierCode:  r.CarrierCode,
			CarrierTitle: r.CarrierTitle,
			MethodCode:   r.MethodCode,
			MethodTitle:  r.MethodTitle,
			Amount:       &model.Money{Value: &price, Currency: &currency},
			Available:    true,
		})
	}
	return result, nil
}

// EstimateTotals returns estimated cart totals for a temporary address and optional shipping method.
func (s *CartService) EstimateTotals(ctx context.Context, input model.EstimateTotalsInput) (*model.EstimateTotalsOutput, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, input.CartID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(input.CartID)
	}
	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(input.CartID)
	}
	items, _ := s.itemRepo.GetByQuoteID(ctx, quoteID)

	// Build a temporary address for the pipeline
	tempAddr := &repository.CartAddressData{
		AddressType: "shipping",
		CountryID:   input.Address.CountryCode,
		RegionID:    input.Address.RegionID,
		Postcode:    input.Address.Postcode,
	}

	// If shipping method specified, estimate its cost
	if input.ShippingMethod != nil {
		storeID := middleware.GetStoreID(ctx)
		req := &shipping.RateRequest{
			StoreID:   storeID,
			WebsiteID: s.cp.GetWebsiteID(storeID),
			CountryID: input.Address.CountryCode,
			RegionID:  input.Address.RegionID,
			Postcode:  input.Address.Postcode,
			Subtotal:  cart.Subtotal,
			ItemQty:   cart.ItemsQty,
		}
		rates := s.shippingRegistry.CollectRates(ctx, req)
		for _, r := range rates {
			if r.CarrierCode == input.ShippingMethod.CarrierCode && r.MethodCode == input.ShippingMethod.MethodCode {
				tempAddr.ShippingAmount = r.Price
				break
			}
		}
	}

	// Run pipeline with temporary address
	cc := &totals.CollectorContext{
		Quote:   cart,
		Items:   items,
		Address: tempAddr,
		StoreID: cart.StoreID,
	}
	total, err := s.pipeline.Collect(ctx, cc)
	if err != nil {
		return nil, err
	}

	currency := model.CurrencyEnum(cart.QuoteCurrencyCode)
	return &model.EstimateTotalsOutput{
		GrandTotal: &model.Money{Value: &total.GrandTotal, Currency: &currency},
		Subtotal:   &model.Money{Value: &total.Subtotal, Currency: &currency},
		Tax:        &model.Money{Value: &total.TaxAmount, Currency: &currency},
		Shipping:   &model.Money{Value: &total.ShippingAmount, Currency: &currency},
		Discount:   &model.Money{Value: &total.DiscountAmount, Currency: &currency},
	}, nil
}

// ApplyCoupon validates and applies a coupon code to the cart.
func (s *CartService) ApplyCoupon(ctx context.Context, maskedID, couponCode string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	// Check if coupon already applied
	if cart.CouponCode != nil && *cart.CouponCode != "" {
		return nil, fmt.Errorf("A coupon is already applied to the cart. Please remove it to apply another")
	}

	// Validate coupon
	websiteID := s.cp.GetWebsiteID(cart.StoreID)
	customerGroupID := 0 // guest
	if cart.CustomerID != nil && *cart.CustomerID > 0 {
		customerGroupID = 1 // General
	}

	_, rule, err := s.couponRepo.LookupCoupon(ctx, couponCode, websiteID, customerGroupID)
	if err != nil {
		return nil, fmt.Errorf("The coupon code isn't valid. Verify the code and try again.")
	}

	// Apply discount to items
	items, _ := s.itemRepo.GetByQuoteID(ctx, quoteID)
	targetSkus := s.couponRepo.GetRuleActionSkus(ctx, rule.RuleID)
	skuSet := make(map[string]bool, len(targetSkus))
	for _, sk := range targetSkus {
		skuSet[sk] = true
	}

	ruleIDStr := fmt.Sprintf("%d", rule.RuleID)
	for _, item := range items {
		if item.ParentItemID != nil {
			continue
		}
		if len(skuSet) > 0 && !skuSet[item.SKU] {
			continue
		}

		var discountAmount float64
		var discountPercent float64
		switch rule.SimpleAction {
		case "by_percent":
			discountPercent = rule.DiscountAmount
			discountAmount = item.RowTotal * discountPercent / 100.0
		case "by_fixed":
			discountAmount = rule.DiscountAmount * item.Qty
		case "cart_fixed":
			var totalSubtotal float64
			for _, it := range items {
				if it.ParentItemID == nil {
					totalSubtotal += it.RowTotal
				}
			}
			if totalSubtotal > 0 {
				discountAmount = rule.DiscountAmount * (item.RowTotal / totalSubtotal)
			}
		}

		discountAmount = math.Round(discountAmount*100) / 100
		if discountAmount > item.RowTotal {
			discountAmount = item.RowTotal
		}

		s.couponRepo.UpdateItemDiscount(ctx, item.ItemID, discountAmount, discountPercent, ruleIDStr)
	}

	// Store coupon on quote
	s.couponRepo.SetCouponOnQuote(ctx, quoteID, couponCode, ruleIDStr)

	// Recalculate totals (pipeline will pick up discount via DiscountCollector)
	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}

	return s.GetCart(ctx, maskedID)
}

// RemoveCoupon removes the coupon from the cart.
func (s *CartService) RemoveCoupon(ctx context.Context, maskedID string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	// Clear coupon and item discounts
	s.couponRepo.ClearCouponOnQuote(ctx, quoteID)
	s.couponRepo.ClearItemDiscounts(ctx, quoteID)

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}

	return s.GetCart(ctx, maskedID)
}

// recalculateTotals runs the totals pipeline and updates the quote.
func (s *CartService) recalculateTotals(ctx context.Context, quoteID int) error {
	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return err
	}

	items, err := s.itemRepo.GetByQuoteID(ctx, quoteID)
	if err != nil {
		return err
	}

	addrs, _ := s.addressRepo.GetByQuoteID(ctx, quoteID)

	// Find shipping address for pipeline context
	var shippingAddr *repository.CartAddressData
	for _, a := range addrs {
		if a.AddressType == "shipping" {
			shippingAddr = a
			break
		}
	}

	// Run totals pipeline
	cc := &totals.CollectorContext{
		Quote:   cart,
		Items:   items,
		Address: shippingAddr,
		StoreID: cart.StoreID,
	}
	total, err := s.pipeline.Collect(ctx, cc)
	if err != nil {
		return fmt.Errorf("totals pipeline: %w", err)
	}

	var itemsQty float64
	for _, item := range items {
		if item.ParentItemID == nil {
			itemsQty += item.Qty
		}
	}

	return s.cartRepo.UpdateTotals(ctx, quoteID, total.Subtotal, total.GrandTotal, total.DiscountAmount, len(items), itemsQty)
}

// collectTotals runs the pipeline without persisting — used for display and order placement.
func (s *CartService) collectTotals(ctx context.Context, cart *repository.CartData, items []*repository.CartItemData, addrs []*repository.CartAddressData) (*totals.Total, error) {
	var shippingAddr *repository.CartAddressData
	for _, a := range addrs {
		if a.AddressType == "shipping" {
			shippingAddr = a
			break
		}
	}

	cc := &totals.CollectorContext{
		Quote:   cart,
		Items:   items,
		Address: shippingAddr,
		StoreID: cart.StoreID,
	}
	return s.pipeline.Collect(ctx, cc)
}

// SetShippingAddresses sets the shipping address on the cart.
func (s *CartService) SetShippingAddresses(ctx context.Context, maskedID string, addresses []*model.ShippingAddressInput) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	for _, addr := range addresses {
		if addr.Address == nil {
			continue
		}
		a := addr.Address
		_, err := s.addressRepo.SetAddress(ctx, quoteID, "shipping",
			a.Firstname, a.Lastname, a.City, a.CountryCode, a.Street,
			a.Company, a.Region, a.Postcode, a.Telephone, a.RegionID,
		)
		if err != nil {
			return nil, fmt.Errorf("Failed to set shipping address: %w", err)
		}
	}

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}
	return s.GetCart(ctx, maskedID)
}

// SetBillingAddress sets the billing address on the cart.
func (s *CartService) SetBillingAddress(ctx context.Context, maskedID string, input *model.BillingAddressInput) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	if input.SameAsShipping != nil && *input.SameAsShipping {
		// Copy shipping address as billing
		addrs, _ := s.addressRepo.GetByQuoteID(ctx, quoteID)
		for _, a := range addrs {
			if a.AddressType == "shipping" {
				street := strings.Split(a.Street, "\n")
				s.addressRepo.SetAddress(ctx, quoteID, "billing",
					a.Firstname, a.Lastname, a.City, a.CountryID, street,
					a.Company, a.Region, a.Postcode, a.Telephone, a.RegionID,
				)
				break
			}
		}
	} else if input.Address != nil {
		a := input.Address
		_, err := s.addressRepo.SetAddress(ctx, quoteID, "billing",
			a.Firstname, a.Lastname, a.City, a.CountryCode, a.Street,
			a.Company, a.Region, a.Postcode, a.Telephone, a.RegionID,
		)
		if err != nil {
			return nil, fmt.Errorf("Failed to set billing address: %w", err)
		}
	}

	return s.GetCart(ctx, maskedID)
}

// SetShippingMethods selects a shipping method on the cart.
func (s *CartService) SetShippingMethods(ctx context.Context, maskedID string, methods []*model.ShippingMethodInput) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	addrs, _ := s.addressRepo.GetByQuoteID(ctx, quoteID)
	cart, _ := s.cartRepo.GetByID(ctx, quoteID)

	for _, method := range methods {
		// Find shipping address
		for _, a := range addrs {
			if a.AddressType == "shipping" {
				// Validate carrier/method via registry
				storeID := middleware.GetStoreID(ctx)
				req := s.buildRateRequest(storeID, a, cart)
				rates := s.shippingRegistry.CollectRates(ctx, req)
				var selectedRate *shipping.Rate
				for _, r := range rates {
					if r.CarrierCode == method.CarrierCode && r.MethodCode == method.MethodCode {
						selectedRate = r
						break
					}
				}
				if selectedRate == nil {
					return nil, carterr.ErrCarrierNotFound(method.CarrierCode + "_" + method.MethodCode)
				}

				desc := selectedRate.CarrierTitle + " - " + selectedRate.MethodTitle
				s.shippingRepo.SetShippingMethod(ctx, a.AddressID, selectedRate.CarrierCode, selectedRate.MethodCode, selectedRate.Price, desc)
				break
			}
		}
	}

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}
	return s.GetCart(ctx, maskedID)
}

// PlaceOrder validates cart state and converts it into an order.
func (s *CartService) PlaceOrder(ctx context.Context, maskedID string) (string, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return "", carterr.ErrCartNotFound(maskedID)
	}

	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return "", carterr.ErrCartNotFound(maskedID)
	}
	if cart.IsActive != 1 {
		return "", carterr.ErrCartNotActive
	}

	// Validate items exist
	items, err := s.itemRepo.GetByQuoteID(ctx, quoteID)
	if err != nil || len(items) == 0 {
		return "", carterr.ErrPlaceOrderFailed
	}

	// Validate addresses
	addrs, err := s.addressRepo.GetByQuoteID(ctx, quoteID)
	if err != nil {
		return "", carterr.ErrPlaceOrderFailed
	}

	var hasShipping, hasBilling bool
	for _, a := range addrs {
		if a.AddressType == "shipping" {
			hasShipping = true
			if a.ShippingMethod == nil || *a.ShippingMethod == "" {
				return "", carterr.ErrShippingMethodMissing
			}
		}
		if a.AddressType == "billing" {
			hasBilling = true
		}
	}

	if cart.IsVirtual != 1 && !hasShipping {
		return "", carterr.ErrAddressInvalid
	}
	if !hasBilling {
		return "", carterr.ErrAddressInvalid
	}

	// Validate payment
	selectedPayment, err := s.paymentRepo.GetSelectedMethod(ctx, quoteID)
	if err != nil || selectedPayment.Code == "" {
		return "", carterr.ErrPaymentMissing
	}

	// Validate guest email
	if cart.CustomerID == nil || *cart.CustomerID == 0 {
		if cart.CustomerEmail == nil || *cart.CustomerEmail == "" {
			return "", carterr.ErrGuestEmailMissing
		}
	}

	// Compute totals for the order using the same pipeline as cart display
	orderTotals, err := s.collectTotals(ctx, cart, items, addrs)
	if err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals collection for order failed")
	}
	var totalTax float64
	if orderTotals != nil {
		totalTax = orderTotals.TaxAmount
	}

	incrementID, err := s.orderRepo.PlaceOrder(ctx, cart, items, addrs, selectedPayment.Code, totalTax)
	if err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("place order failed")
		return "", carterr.ErrPlaceOrderFailed
	}

	log.Info().Str("increment_id", incrementID).Int("quote_id", quoteID).Msg("order placed")
	return incrementID, nil
}

// ── Mapping ─────────────────────────────────────────────────────────────────

// SetPaymentMethod sets the payment method on the cart.
func (s *CartService) SetPaymentMethod(ctx context.Context, maskedID string, methodCode string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	// Validate method
	storeID := middleware.GetStoreID(ctx)
	cart, _ := s.cartRepo.GetByID(ctx, quoteID)
	available := s.paymentRepo.GetAvailableMethods(ctx, storeID, cart.GrandTotal)
	found := false
	for _, m := range available {
		if m.Code == methodCode {
			found = true
			break
		}
	}
	if !found {
		return nil, carterr.ErrPaymentNotAvailable
	}

	s.paymentRepo.SetPaymentMethod(ctx, quoteID, methodCode)
	return s.GetCart(ctx, maskedID)
}

func (s *CartService) mapCart(ctx context.Context, cart *repository.CartData, maskedID string) (*model.Cart, error) {
	items, _ := s.itemRepo.GetByQuoteID(ctx, cart.EntityID)
	currency := model.CurrencyEnum(cart.QuoteCurrencyCode)

	totalQty := cart.ItemsQty
	isVirtual := cart.IsVirtual == 1

	cartItems := make([]model.CartItemInterface, 0, len(items))
	for _, item := range items {
		if item.ParentItemID != nil {
			continue // skip child items
		}
		cartItems = append(cartItems, s.mapCartItem(ctx, item, items, currency))
	}

	// Load addresses
	addrs, _ := s.addressRepo.GetByQuoteID(ctx, cart.EntityID)
	storeID := middleware.GetStoreID(ctx)

	// Compute totals via pipeline (single source of truth for tax/totals)
	displayTotals, _ := s.collectTotals(ctx, cart, items, addrs)
	var totalTax float64
	var appliedTaxes []*model.CartTaxItem
	if displayTotals != nil {
		totalTax = displayTotals.TaxAmount
		for _, at := range displayTotals.AppliedTaxes {
			amt := at.Amount
			appliedTaxes = append(appliedTaxes, &model.CartTaxItem{
				Amount: &model.Money{Value: &amt, Currency: &currency},
				Label:  at.Label,
			})
		}
	}

	subtotalInclTax := cart.Subtotal + totalTax

	// Compute discount for display
	var discountAmount float64
	if displayTotals != nil {
		discountAmount = displayTotals.DiscountAmount
	}
	subtotalWithDiscount := math.Round((cart.Subtotal-discountAmount)*100) / 100

	var discounts []*model.Discount
	if discountAmount > 0 {
		label := "Discount"
		if cart.CouponCode != nil && *cart.CouponCode != "" {
			label = *cart.CouponCode
		}
		discounts = append(discounts, &model.Discount{
			Amount: &model.Money{Value: &discountAmount, Currency: &currency},
			Label:  label,
		})
	}

	result := &model.Cart{
		ID:            maskedID,
		Items:         cartItems,
		TotalQuantity: totalQty,
		IsVirtual:     isVirtual,
		Email:         cart.CustomerEmail,
		Prices: &model.CartPrices{
			GrandTotal:                          &model.Money{Value: &cart.GrandTotal, Currency: &currency},
			SubtotalExcludingTax:                &model.Money{Value: &cart.Subtotal, Currency: &currency},
			SubtotalIncludingTax:                &model.Money{Value: &subtotalInclTax, Currency: &currency},
			SubtotalWithDiscountExcludingTax:     &model.Money{Value: &subtotalWithDiscount, Currency: &currency},
			AppliedTaxes:                         appliedTaxes,
			Discounts:                            discounts,
		},
		ShippingAddresses: []*model.ShippingCartAddress{},
	}
	for _, a := range addrs {
		switch a.AddressType {
		case "shipping":
			sa := s.mapShippingAddress(ctx, a)
			// Load available shipping methods via carrier registry
			req := s.buildRateRequest(storeID, a, cart)
			rates := s.shippingRegistry.CollectRates(ctx, req)
			for _, r := range rates {
				price := r.Price
				sa.AvailableShippingMethods = append(sa.AvailableShippingMethods, &model.AvailableShippingMethod{
					CarrierCode:  r.CarrierCode,
					CarrierTitle: r.CarrierTitle,
					MethodCode:   r.MethodCode,
					MethodTitle:  r.MethodTitle,
					Amount:       &model.Money{Value: &price, Currency: &currency},
					Available:    true,
				})
			}
			result.ShippingAddresses = append(result.ShippingAddresses, sa)
		case "billing":
			result.BillingAddress = s.mapBillingAddress(ctx, a)
		}
	}

	if cart.CouponCode != nil {
		result.AppliedCoupons = []*model.AppliedCoupon{{Code: *cart.CouponCode}}
	}

	// Available payment methods
	availablePayments := s.paymentRepo.GetAvailableMethods(ctx, storeID, cart.GrandTotal)
	for _, pm := range availablePayments {
		result.AvailablePaymentMethods = append(result.AvailablePaymentMethods, &model.AvailablePaymentMethod{
			Code: pm.Code, Title: pm.Title,
		})
	}

	// Selected payment method
	if selected, err := s.paymentRepo.GetSelectedMethod(ctx, cart.EntityID); err == nil {
		result.SelectedPaymentMethod = &model.SelectedPaymentMethod{
			Code: selected.Code, Title: &selected.Title,
		}
	}

	return result, nil
}

func (s *CartService) mapShippingAddress(ctx context.Context, a *repository.CartAddressData) *model.ShippingCartAddress {
	addr := &model.ShippingCartAddress{
		Firstname: a.Firstname,
		Lastname:  a.Lastname,
		Street:    toStringPtrs(strings.Split(a.Street, "\n")),
		City:      a.City,
		Postcode:  a.Postcode,
		Company:   a.Company,
		Telephone: a.Telephone,
		Country:   &model.CartAddressCountry{Code: a.CountryID, Label: a.CountryID},
	}
	if a.RegionID != nil {
		code, name, err := s.addressRepo.ResolveRegion(ctx, *a.RegionID)
		if err == nil {
			addr.Region = &model.CartAddressRegion{Code: &code, Label: &name, RegionID: a.RegionID}
		}
	} else if a.Region != nil {
		addr.Region = &model.CartAddressRegion{Label: a.Region}
	}
	if a.ShippingMethod != nil {
		parts := strings.SplitN(*a.ShippingMethod, "_", 2)
		if len(parts) == 2 {
			desc := ""
			if a.ShippingDescription != nil {
				desc = *a.ShippingDescription
			}
			addr.SelectedShippingMethod = &model.SelectedShippingMethod{
				CarrierCode:  parts[0],
				CarrierTitle: parts[0],
				MethodCode:   parts[1],
				MethodTitle:  desc,
				Amount:       &model.Money{Value: &a.ShippingAmount, Currency: nil},
			}
		}
	}
	return addr
}

func (s *CartService) mapBillingAddress(ctx context.Context, a *repository.CartAddressData) *model.BillingCartAddress {
	addr := &model.BillingCartAddress{
		Firstname: a.Firstname,
		Lastname:  a.Lastname,
		Street:    toStringPtrs(strings.Split(a.Street, "\n")),
		City:      a.City,
		Postcode:  a.Postcode,
		Company:   a.Company,
		Telephone: a.Telephone,
		Country:   &model.CartAddressCountry{Code: a.CountryID, Label: a.CountryID},
	}
	if a.RegionID != nil {
		code, name, err := s.addressRepo.ResolveRegion(ctx, *a.RegionID)
		if err == nil {
			addr.Region = &model.CartAddressRegion{Code: &code, Label: &name, RegionID: a.RegionID}
		}
	}
	return addr
}

func (s *CartService) mapCartItem(ctx context.Context, item *repository.CartItemData, allItems []*repository.CartItemData, currency model.CurrencyEnum) model.CartItemInterface {
	uid := encodeUID(item.ItemID)
	prices := &model.CartItemPrices{
		Price:                &model.Money{Value: &item.Price, Currency: &currency},
		RowTotal:             &model.Money{Value: &item.RowTotal, Currency: &currency},
		RowTotalIncludingTax: &model.Money{Value: &item.RowTotal, Currency: &currency},
	}
	if item.DiscountAmount > 0 {
		prices.TotalItemDiscount = &model.Money{Value: &item.DiscountAmount, Currency: &currency}
		prices.Discounts = []*model.Discount{{
			Amount: &model.Money{Value: &item.DiscountAmount, Currency: &currency},
			Label:  "Discount",
		}}
	}

	if item.ProductType == "configurable" {
		return s.mapConfigurableCartItem(ctx, item, allItems, uid, prices, currency)
	}

	if item.ProductType == "bundle" {
		return s.mapBundleCartItem(ctx, item, allItems, uid, prices, currency)
	}

	return &model.SimpleCartItem{
		UID:      uid,
		Quantity: item.Qty,
		Product: &model.CartItemProduct{
			Sku:  item.SKU,
			Name: &item.Name,
		},
		Prices: prices,
	}
}

func (s *CartService) mapConfigurableCartItem(ctx context.Context, item *repository.CartItemData, allItems []*repository.CartItemData, uid string, prices *model.CartItemPrices, currency model.CurrencyEnum) *model.ConfigurableCartItem {
	db := s.cartRepo.DB()

	// Find the parent product's original SKU (quote_item.sku = child sku for configurables)
	var parentSKU string
	db.QueryRowContext(ctx,
		"SELECT sku FROM catalog_product_entity WHERE entity_id = ?",
		item.ProductID,
	).Scan(&parentSKU)

	// Find child item in allItems
	var childItem *repository.CartItemData
	for _, ci := range allItems {
		if ci.ParentItemID != nil && *ci.ParentItemID == item.ItemID {
			childItem = ci
			break
		}
	}

	// Resolve configurable options from super attributes
	var configOptions []*model.SelectedConfigurableOption
	rows, err := db.QueryContext(ctx, `
		SELECT cpsa.attribute_id, ea.attribute_code
		FROM catalog_product_super_attribute cpsa
		JOIN eav_attribute ea ON cpsa.attribute_id = ea.attribute_id
		WHERE cpsa.product_id = ?
		ORDER BY cpsa.position`, item.ProductID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var attrID int
			var attrCode string
			rows.Scan(&attrID, &attrCode)

			if childItem != nil {
				// Get the option value from the child product
				var optionID int
				db.QueryRowContext(ctx,
					"SELECT value FROM catalog_product_entity_int WHERE entity_id = ? AND attribute_id = ? AND store_id = 0",
					childItem.ProductID, attrID,
				).Scan(&optionID)

				// Get option label
				var optionLabel string
				db.QueryRowContext(ctx,
					"SELECT value FROM eav_attribute_option_value WHERE option_id = ? AND store_id = 0",
					optionID,
				).Scan(&optionLabel)

				// Get attribute frontend label
				var attrLabel string
				db.QueryRowContext(ctx,
					"SELECT COALESCE(frontend_label, attribute_code) FROM eav_attribute WHERE attribute_id = ?",
					attrID,
				).Scan(&attrLabel)

				configOptions = append(configOptions, &model.SelectedConfigurableOption{
					ID:         attrID,
					OptionLabel: attrLabel,
					ValueID:    optionID,
					ValueLabel: optionLabel,
				})
			}
		}
	}

	result := &model.ConfigurableCartItem{
		UID:      uid,
		Quantity: item.Qty,
		Product: &model.CartItemProduct{
			Sku:  parentSKU,
			Name: &item.Name,
		},
		Prices:              prices,
		ConfigurableOptions: configOptions,
	}

	// Add configured_variant (child product info)
	if childItem != nil {
		result.ConfiguredVariant = &model.CartItemProduct{
			Sku:  childItem.SKU,
			Name: &childItem.Name,
		}
	}

	return result
}

func (s *CartService) mapBundleCartItem(ctx context.Context, item *repository.CartItemData, allItems []*repository.CartItemData, uid string, prices *model.CartItemPrices, currency model.CurrencyEnum) *model.BundleCartItem {
	db := s.cartRepo.DB()

	// Find child items
	var childItems []*repository.CartItemData
	for _, ci := range allItems {
		if ci.ParentItemID != nil && *ci.ParentItemID == item.ItemID {
			childItems = append(childItems, ci)
		}
	}

	// Look up bundle options to build bundle_options response
	var bundleOptions []*model.SelectedBundleOption
	rows, err := db.QueryContext(ctx, `
		SELECT bo.option_id, COALESCE(bov.title, ''), bo.type
		FROM catalog_product_bundle_option bo
		LEFT JOIN catalog_product_bundle_option_value bov ON bo.option_id = bov.option_id AND bov.store_id = 0
		WHERE bo.parent_id = ?
		ORDER BY bo.position`, item.ProductID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var optionID int
			var title, optType string
			rows.Scan(&optionID, &title, &optType)

			// Find which child maps to this option
			var values []*model.SelectedBundleOptionValue
			for _, child := range childItems {
				// Look up which selection this child product corresponds to
				var selectionID int
				var selQty float64
				err := db.QueryRowContext(ctx,
					"SELECT selection_id, selection_qty FROM catalog_product_bundle_selection WHERE parent_product_id = ? AND option_id = ? AND product_id = ?",
					item.ProductID, optionID, child.ProductID,
				).Scan(&selectionID, &selQty)
				if err != nil {
					continue
				}

				// Get child product name
				childPrice := child.Price
				if childPrice == 0 {
					// For child items with price=0, look up from catalog
					cp, _ := s.lookupProduct(ctx, child.SKU, 0)
					if cp != nil {
						childPrice = cp.Price
					}
				}

				values = append(values, &model.SelectedBundleOptionValue{
					ID:       selectionID,
					Label:    child.Name,
					Quantity: child.Qty / item.Qty, // per-bundle qty
					Price:    childPrice,
				})
			}

			if len(values) > 0 {
				optionUID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("bundle/%d", optionID)))
				bundleOptions = append(bundleOptions, &model.SelectedBundleOption{
					UID:    optionUID,
					Label:  title,
					Type:   optType,
					Values: values,
				})
			}
		}
	}

	// Get parent product's original SKU
	var parentSKU string
	db.QueryRowContext(ctx,
		"SELECT sku FROM catalog_product_entity WHERE entity_id = ?",
		item.ProductID,
	).Scan(&parentSKU)

	return &model.BundleCartItem{
		UID:      uid,
		Quantity: item.Qty,
		Product: &model.CartItemProduct{
			Sku:  parentSKU,
			Name: &item.Name,
		},
		Prices:        prices,
		BundleOptions: bundleOptions,
	}
}

func encodeUID(id int) string {
	return base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(id)))
}

func decodeUID(uid string) (int, error) {
	decoded, err := base64.StdEncoding.DecodeString(uid)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(decoded))
}

func toStringPtrs(ss []string) []*string {
	result := make([]*string, len(ss))
	for i := range ss {
		result[i] = &ss[i]
	}
	return result
}

func (s *CartService) buildRateRequest(storeID int, addr *repository.CartAddressData, cart *repository.CartData) *shipping.RateRequest {
	return &shipping.RateRequest{
		StoreID:   storeID,
		WebsiteID: s.cp.GetWebsiteID(storeID),
		CountryID: addr.CountryID,
		RegionID:  addr.RegionID,
		Postcode:  addr.Postcode,
		Subtotal:  cart.Subtotal,
		ItemQty:   cart.ItemsQty,
	}
}

// productInfo holds the result of a product lookup.
type productInfo struct {
	ProductID   int
	ProductType string
	Name        string
	Price       float64
	Status      int
	StockStatus int
}

func (s *CartService) lookupProduct(ctx context.Context, sku string, storeID int) (*productInfo, error) {
	var p productInfo
	err := s.cartRepo.DB().QueryRowContext(ctx, `
		SELECT cpe.entity_id, cpe.type_id,
		       COALESCE(cpev.value, cpe.sku) as name,
		       COALESCE(cpip.final_price, 0) as price,
		       COALESCE(cpei_status.value, 1) as status,
		       COALESCE(csi.is_in_stock, 1) as stock_status
		FROM catalog_product_entity cpe
		LEFT JOIN catalog_product_entity_varchar cpev
			ON cpe.entity_id = cpev.entity_id
			AND cpev.attribute_id = (SELECT attribute_id FROM eav_attribute WHERE attribute_code = 'name' AND entity_type_id = 4)
			AND cpev.store_id = 0
		LEFT JOIN catalog_product_index_price cpip
			ON cpe.entity_id = cpip.entity_id AND cpip.customer_group_id = 0
			AND cpip.website_id = (SELECT website_id FROM store WHERE store_id = ? LIMIT 1)
		LEFT JOIN catalog_product_entity_int cpei_status
			ON cpe.entity_id = cpei_status.entity_id
			AND cpei_status.attribute_id = (SELECT attribute_id FROM eav_attribute WHERE attribute_code = 'status' AND entity_type_id = 4)
			AND cpei_status.store_id = 0
		LEFT JOIN cataloginventory_stock_item csi ON cpe.entity_id = csi.product_id
		WHERE cpe.sku = ?`,
		storeID, sku,
	).Scan(&p.ProductID, &p.ProductType, &p.Name, &p.Price, &p.Status, &p.StockStatus)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// addConfigurableProduct handles adding a configurable product to the cart.
// It decodes selected_options, finds the matching child product, and inserts
// both parent (configurable, carries price) and child (simple, price=0) rows.
func (s *CartService) addConfigurableProduct(ctx context.Context, quoteID, storeID int, parent *productInfo, input *model.CartItemInput) error {
	db := s.cartRepo.DB()

	// Decode selected_options: base64("configurable/<attr_id>/<option_id>")
	superAttributes := make(map[int]int) // attribute_id → option_id
	for _, opt := range input.SelectedOptions {
		decoded, err := base64.StdEncoding.DecodeString(opt)
		if err != nil {
			return fmt.Errorf("You need to choose options for your item.")
		}
		parts := strings.Split(string(decoded), "/")
		if len(parts) != 3 || parts[0] != "configurable" {
			return fmt.Errorf("You need to choose options for your item.")
		}
		attrID, _ := strconv.Atoi(parts[1])
		optionID, _ := strconv.Atoi(parts[2])
		if attrID == 0 || optionID == 0 {
			return fmt.Errorf("You need to choose options for your item.")
		}
		superAttributes[attrID] = optionID
	}

	if len(superAttributes) == 0 {
		return fmt.Errorf("You need to choose options for your item.")
	}

	// Find all child products for this configurable
	rows, err := db.QueryContext(ctx, `
		SELECT cpsl.product_id, cpe.sku
		FROM catalog_product_super_link cpsl
		JOIN catalog_product_entity cpe ON cpsl.product_id = cpe.entity_id
		WHERE cpsl.parent_id = ?`, parent.ProductID)
	if err != nil {
		return fmt.Errorf("Could not find child products for \"%s\"", input.Sku)
	}
	defer rows.Close()

	type childCandidate struct {
		productID int
		sku       string
	}
	var candidates []childCandidate
	for rows.Next() {
		var c childCandidate
		rows.Scan(&c.productID, &c.sku)
		candidates = append(candidates, c)
	}

	// Find the child matching ALL selected attribute/option pairs
	var matchedChild *childCandidate
	for i := range candidates {
		allMatch := true
		for attrID, optionID := range superAttributes {
			var val int
			err := db.QueryRowContext(ctx,
				"SELECT value FROM catalog_product_entity_int WHERE entity_id = ? AND attribute_id = ? AND store_id = 0",
				candidates[i].productID, attrID,
			).Scan(&val)
			if err != nil || val != optionID {
				allMatch = false
				break
			}
		}
		if allMatch {
			matchedChild = &candidates[i]
			break
		}
	}

	if matchedChild == nil {
		return fmt.Errorf("The requested product is not available with the selected options.")
	}

	// Look up child product details (name, price, stock)
	child, err := s.lookupProduct(ctx, matchedChild.sku, storeID)
	if err != nil {
		return fmt.Errorf("Could not find product \"%s\"", matchedChild.sku)
	}
	if child.StockStatus != 1 {
		return fmt.Errorf("Product \"%s\" is out of stock.", input.Sku)
	}

	// The price comes from the child product in the price index
	price := child.Price

	// Insert parent row (configurable type, carries price)
	// SKU = child's SKU (matching Magento behavior)
	parentItemID, err := s.itemRepo.AddConfigurable(ctx, quoteID, parent.ProductID, matchedChild.sku, parent.Name, "configurable", input.Quantity, price)
	if err != nil {
		return fmt.Errorf("Could not add \"%s\" to cart: %v", input.Sku, err)
	}

	// Insert child row (simple type, price=0, parent_item_id)
	_, err = s.itemRepo.AddChild(ctx, quoteID, child.ProductID, matchedChild.sku, child.Name, "simple", input.Quantity, parentItemID)
	if err != nil {
		return fmt.Errorf("Could not add \"%s\" to cart: %v", input.Sku, err)
	}

	return nil
}

// addBundleProduct handles adding a bundle product to the cart.
// Decodes selected_options (bundle/<option_id>/<selection_id>/<qty>),
// looks up each selection's child product, inserts parent (bundle, total price)
// + children (simple, individual prices).
func (s *CartService) addBundleProduct(ctx context.Context, quoteID, storeID int, parent *productInfo, input *model.CartItemInput) error {
	db := s.cartRepo.DB()

	// Decode selected_options: base64("bundle/<option_id>/<selection_id>/<qty>")
	type bundleSelection struct {
		optionID    int
		selectionID int
		qty         float64
	}
	var selections []bundleSelection
	for _, opt := range input.SelectedOptions {
		decoded, err := base64.StdEncoding.DecodeString(opt)
		if err != nil {
			return fmt.Errorf("You need to choose options for your item.")
		}
		parts := strings.Split(string(decoded), "/")
		if len(parts) < 3 || parts[0] != "bundle" {
			continue // skip non-bundle options
		}
		optID, _ := strconv.Atoi(parts[1])
		selID, _ := strconv.Atoi(parts[2])
		qty := 1.0
		if len(parts) >= 4 {
			if q, err := strconv.ParseFloat(parts[3], 64); err == nil {
				qty = q
			}
		}
		if optID == 0 || selID == 0 {
			return fmt.Errorf("You need to choose options for your item.")
		}
		selections = append(selections, bundleSelection{optionID: optID, selectionID: selID, qty: qty})
	}

	if len(selections) == 0 {
		return fmt.Errorf("You need to choose options for your item.")
	}

	// Look up each selection's child product and compute total price
	type childInfo struct {
		selectionID int
		optionID    int
		productID   int
		sku         string
		name        string
		price       float64
		qty         float64
	}
	var children []childInfo
	var totalPrice float64
	var childSkus []string

	for _, sel := range selections {
		var productID int
		var sku string
		err := db.QueryRowContext(ctx,
			"SELECT product_id, (SELECT sku FROM catalog_product_entity WHERE entity_id = bs.product_id) FROM catalog_product_bundle_selection bs WHERE bs.selection_id = ? AND bs.parent_product_id = ?",
			sel.selectionID, parent.ProductID,
		).Scan(&productID, &sku)
		if err != nil {
			return fmt.Errorf("Invalid bundle selection %d", sel.selectionID)
		}

		childProduct, err := s.lookupProduct(ctx, sku, storeID)
		if err != nil {
			return fmt.Errorf("Could not find product \"%s\"", sku)
		}

		childPrice := childProduct.Price * sel.qty
		totalPrice += childPrice
		childSkus = append(childSkus, sku)

		children = append(children, childInfo{
			selectionID: sel.selectionID,
			optionID:    sel.optionID,
			productID:   productID,
			sku:         sku,
			name:        childProduct.Name,
			price:       childProduct.Price,
			qty:         sel.qty,
		})
	}

	// Build composite SKU: parent-child1-child2-...
	compositeSKU := parent.Name // Magento uses parent SKU + child SKUs joined by -
	compositeSKU = input.Sku
	for _, sku := range childSkus {
		compositeSKU += "-" + sku
	}

	// Insert parent row (bundle type, total price)
	parentItemID, err := s.itemRepo.AddConfigurable(ctx, quoteID, parent.ProductID, compositeSKU, parent.Name, "bundle", input.Quantity, totalPrice)
	if err != nil {
		return fmt.Errorf("Could not add \"%s\" to cart: %v", input.Sku, err)
	}

	// Insert child rows (simple type, individual prices, parent_item_id)
	for _, child := range children {
		s.itemRepo.AddChild(ctx, quoteID, child.productID, child.sku, child.name, "simple", child.qty*input.Quantity, parentItemID)
	}

	return nil
}
