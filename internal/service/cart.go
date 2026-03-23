package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/magendooro/magento2-cart-graphql-go/graph/model"
	"github.com/magendooro/magento2-cart-graphql-go/internal/config"
	"github.com/magendooro/magento2-cart-graphql-go/internal/middleware"
	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
	"github.com/magendooro/magento2-cart-graphql-go/internal/totals"
)

type CartService struct {
	cartRepo     *repository.CartRepository
	maskRepo     *repository.CartMaskRepository
	itemRepo     *repository.CartItemRepository
	addressRepo  *repository.CartAddressRepository
	shippingRepo *repository.ShippingRepository
	paymentRepo  *repository.PaymentRepository
	taxRepo      *repository.TaxRepository
	orderRepo    *repository.OrderRepository
	pipeline     *totals.Pipeline
	cp           *config.ConfigProvider
}

func NewCartService(
	cartRepo *repository.CartRepository,
	maskRepo *repository.CartMaskRepository,
	itemRepo *repository.CartItemRepository,
	addressRepo *repository.CartAddressRepository,
	shippingRepo *repository.ShippingRepository,
	paymentRepo *repository.PaymentRepository,
	taxRepo *repository.TaxRepository,
	orderRepo *repository.OrderRepository,
	cp *config.ConfigProvider,
) *CartService {
	// Build totals pipeline (order matches Magento's sales.xml sort_order)
	pipeline := totals.NewPipeline(
		&totals.SubtotalCollector{},            // 100
		// &totals.DiscountCollector{},          // 300 — Phase 2 (#10)
		&totals.ShippingCollector{},             // 350
		// &totals.ShippingTaxCollector{},       // 375 — Phase 3 (#21)
		&totals.TaxCollector{TaxRepo: taxRepo},  // 450
		&totals.GrandTotalCollector{},           // 550
	)

	return &CartService{
		cartRepo:     cartRepo,
		maskRepo:     maskRepo,
		itemRepo:     itemRepo,
		addressRepo:  addressRepo,
		shippingRepo: shippingRepo,
		paymentRepo:  paymentRepo,
		taxRepo:      taxRepo,
		orderRepo:    orderRepo,
		pipeline:     pipeline,
		cp:           cp,
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
		return nil, fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
	}

	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil, err
	}
	if cart.IsActive != 1 {
		return nil, fmt.Errorf("The cart isn't active.")
	}

	// Auth check: customer carts require matching customer
	customerID := middleware.GetCustomerID(ctx)
	if cart.CustomerID != nil && *cart.CustomerID > 0 && *cart.CustomerID != customerID {
		return nil, fmt.Errorf("The current user cannot perform operations on cart \"%s\"", maskedID)
	}

	return s.mapCart(ctx, cart, maskedID)
}

// GetCustomerCart fetches the active cart for the authenticated customer.
func (s *CartService) GetCustomerCart(ctx context.Context) (*model.Cart, error) {
	customerID := middleware.GetCustomerID(ctx)
	if customerID == 0 {
		return nil, fmt.Errorf("The current customer isn't authorized.")
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

// AddProducts adds simple products to the cart.
func (s *CartService) AddProducts(ctx context.Context, maskedID string, items []*model.CartItemInput) (*model.AddProductsToCartOutput, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
	}

	var userErrors []*model.CartUserInputError

	for _, input := range items {
		// Look up product by SKU
		var productID int
		var name, productType string
		var price float64
		var status, stockStatus int

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
			middleware.GetStoreID(ctx), input.Sku,
		).Scan(&productID, &productType, &name, &price, &status, &stockStatus)

		if err != nil {
			userErrors = append(userErrors, &model.CartUserInputError{
				Code:    model.CartUserInputErrorTypeProductNotFound,
				Message: fmt.Sprintf("Could not find a product with SKU \"%s\"", input.Sku),
			})
			continue
		}

		if status != 1 {
			userErrors = append(userErrors, &model.CartUserInputError{
				Code:    model.CartUserInputErrorTypeNotSalable,
				Message: fmt.Sprintf("Product \"%s\" is not available for purchase.", input.Sku),
			})
			continue
		}

		if stockStatus != 1 {
			userErrors = append(userErrors, &model.CartUserInputError{
				Code:    model.CartUserInputErrorTypeInsufficientStock,
				Message: fmt.Sprintf("Product \"%s\" is out of stock.", input.Sku),
			})
			continue
		}

		_, err = s.itemRepo.Add(ctx, quoteID, productID, input.Sku, name, productType, input.Quantity, price)
		if err != nil {
			userErrors = append(userErrors, &model.CartUserInputError{
				Code:    model.CartUserInputErrorTypeUndefined,
				Message: fmt.Sprintf("Could not add \"%s\" to cart: %v", input.Sku, err),
			})
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
		return nil, fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
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
		return nil, fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
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
		return nil, fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
	}
	s.cartRepo.UpdateEmail(ctx, quoteID, email)
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

	return s.cartRepo.UpdateTotals(ctx, quoteID, total.Subtotal, total.GrandTotal, len(items), itemsQty)
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
		return nil, fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
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
		return nil, fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
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
		return nil, fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
	}

	addrs, _ := s.addressRepo.GetByQuoteID(ctx, quoteID)
	cart, _ := s.cartRepo.GetByID(ctx, quoteID)

	for _, method := range methods {
		// Find shipping address
		for _, a := range addrs {
			if a.AddressType == "shipping" {
				// Validate carrier/method
				storeID := middleware.GetStoreID(ctx)
				rates, _ := s.shippingRepo.GetAvailableRates(ctx, storeID, a.CountryID, a.RegionID, a.Postcode, cart.Subtotal, cart.ItemsQty)
				var selectedRate *repository.ShippingRate
				for _, r := range rates {
					if r.CarrierCode == method.CarrierCode && r.MethodCode == method.MethodCode {
						selectedRate = r
						break
					}
				}
				if selectedRate == nil {
					return nil, fmt.Errorf("Carrier with such method not found: %s_%s", method.CarrierCode, method.MethodCode)
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
		return "", fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
	}

	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return "", fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
	}
	if cart.IsActive != 1 {
		return "", fmt.Errorf("The cart isn't active.")
	}

	// Validate items exist
	items, err := s.itemRepo.GetByQuoteID(ctx, quoteID)
	if err != nil || len(items) == 0 {
		return "", fmt.Errorf("Unable to place order: A server error stopped your order from being placed. Please try to place your order again.")
	}

	// Validate addresses
	addrs, err := s.addressRepo.GetByQuoteID(ctx, quoteID)
	if err != nil {
		return "", fmt.Errorf("Unable to place order: A server error stopped your order from being placed. Please try to place your order again.")
	}

	var hasShipping, hasBilling bool
	for _, a := range addrs {
		if a.AddressType == "shipping" {
			hasShipping = true
			if a.ShippingMethod == nil || *a.ShippingMethod == "" {
				return "", fmt.Errorf("The shipping method is missing. Select the shipping method and try again.")
			}
		}
		if a.AddressType == "billing" {
			hasBilling = true
		}
	}

	if cart.IsVirtual != 1 && !hasShipping {
		return "", fmt.Errorf("Some addresses can't be used due to the configurations for specific countries.")
	}
	if !hasBilling {
		return "", fmt.Errorf("Some addresses can't be used due to the configurations for specific countries.")
	}

	// Validate payment
	selectedPayment, err := s.paymentRepo.GetSelectedMethod(ctx, quoteID)
	if err != nil || selectedPayment.Code == "" {
		return "", fmt.Errorf("Enter a valid payment method and try again.")
	}

	// Validate guest email
	if cart.CustomerID == nil || *cart.CustomerID == 0 {
		if cart.CustomerEmail == nil || *cart.CustomerEmail == "" {
			return "", fmt.Errorf("Guest email for cart is missing.")
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
		return "", fmt.Errorf("Unable to place order: A server error stopped your order from being placed. Please try to place your order again.")
	}

	log.Info().Str("increment_id", incrementID).Int("quote_id", quoteID).Msg("order placed")
	return incrementID, nil
}

// ── Mapping ─────────────────────────────────────────────────────────────────

// SetPaymentMethod sets the payment method on the cart.
func (s *CartService) SetPaymentMethod(ctx context.Context, maskedID string, methodCode string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, fmt.Errorf("Could not find a cart with ID \"%s\"", maskedID)
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
		return nil, fmt.Errorf("The requested Payment Method is not available.")
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
		cartItems = append(cartItems, s.mapCartItem(item, currency))
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

	result := &model.Cart{
		ID:            maskedID,
		Items:         cartItems,
		TotalQuantity: totalQty,
		IsVirtual:     isVirtual,
		Email:         cart.CustomerEmail,
		Prices: &model.CartPrices{
			GrandTotal:           &model.Money{Value: &cart.GrandTotal, Currency: &currency},
			SubtotalExcludingTax: &model.Money{Value: &cart.Subtotal, Currency: &currency},
			SubtotalIncludingTax: &model.Money{Value: &subtotalInclTax, Currency: &currency},
			AppliedTaxes:         appliedTaxes,
		},
		ShippingAddresses: []*model.ShippingCartAddress{},
	}
	for _, a := range addrs {
		switch a.AddressType {
		case "shipping":
			sa := s.mapShippingAddress(ctx, a)
			// Load available shipping methods
			rates, _ := s.shippingRepo.GetAvailableRates(ctx, storeID, a.CountryID, a.RegionID, a.Postcode, cart.Subtotal, cart.ItemsQty)
			for _, r := range rates {
				available := true
				sa.AvailableShippingMethods = append(sa.AvailableShippingMethods, &model.AvailableShippingMethod{
					CarrierCode:  r.CarrierCode,
					CarrierTitle: r.CarrierTitle,
					MethodCode:   r.MethodCode,
					MethodTitle:  r.MethodTitle,
					Amount:       &model.Money{Value: &r.Price, Currency: &currency},
					Available:    available,
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

func (s *CartService) mapCartItem(item *repository.CartItemData, currency model.CurrencyEnum) model.CartItemInterface {
	uid := encodeUID(item.ItemID)
	return &model.SimpleCartItem{
		UID:      uid,
		Quantity: item.Qty,
		Product: &model.CartItemProduct{
			Sku:  item.SKU,
			Name: &item.Name,
		},
		Prices: &model.CartItemPrices{
			Price:              &model.Money{Value: &item.Price, Currency: &currency},
			RowTotal:           &model.Money{Value: &item.RowTotal, Currency: &currency},
			RowTotalIncludingTax: &model.Money{Value: &item.RowTotal, Currency: &currency},
		},
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
