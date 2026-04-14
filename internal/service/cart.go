package service

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/magendooro/magento2-cart-graphql-go/graph/model"
	"github.com/magendooro/magento2-go-common/config"
	carterr "github.com/magendooro/magento2-cart-graphql-go/internal/errors"
	"github.com/magendooro/magento2-go-common/mgerrors"
	cartmapper "github.com/magendooro/magento2-cart-graphql-go/internal/mapper"
	"github.com/magendooro/magento2-cart-graphql-go/internal/ctxkeys"
	"github.com/magendooro/magento2-go-common/middleware"
	"github.com/magendooro/magento2-cart-graphql-go/internal/order"
	"github.com/magendooro/magento2-cart-graphql-go/internal/payment"
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
	mapper           *cartmapper.CartMapper
	stripeClient     *payment.StripeClient
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
	pipeline := totals.NewPipeline(
		&totals.SubtotalCollector{},
		&totals.DiscountCollector{CouponRepo: couponRepo},
		&totals.ShippingCollector{},
		&totals.ShippingTaxCollector{TaxRepo: taxRepo, CP: cp},
		&totals.TaxCollector{TaxRepo: taxRepo, CP: cp},
		&totals.GrandTotalCollector{},
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
		mapper:           cartmapper.NewCartMapper(cartRepo.DB(), addressRepo),
	}
}

// SetStripeClient wires the Stripe client after construction (avoids circular deps).
func (s *CartService) SetStripeClient(sc *payment.StripeClient) {
	s.stripeClient = sc
}

// ── Cart lifecycle ────────────────────────────────────────────────────────────

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

	customerID := middleware.GetCustomerID(ctx)
	if cart.CustomerID != nil && *cart.CustomerID > 0 && *cart.CustomerID != customerID {
		return nil, carterr.ErrCartForbidden(maskedID)
	}

	return s.buildCart(ctx, cart, maskedID)
}

func (s *CartService) GetCustomerCart(ctx context.Context) (*model.Cart, error) {
	customerID := middleware.GetCustomerID(ctx)
	if customerID == 0 {
		return nil, mgerrors.ErrUnauthorized
	}

	storeID := middleware.GetStoreID(ctx)
	cart, err := s.cartRepo.GetActiveByCustomerID(ctx, customerID, storeID)
	if err != nil {
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

	return s.buildCart(ctx, cart, maskedID)
}

// ── Item operations ───────────────────────────────────────────────────────────

func (s *CartService) AddProducts(ctx context.Context, maskedID string, items []*model.CartItemInput) (*model.AddProductsToCartOutput, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}
	customerGroupID := cart.CustomerGroupID

	storeID := middleware.GetStoreID(ctx)
	var userErrors []*model.CartUserInputError

	for _, input := range items {
		product, err := s.lookupProduct(ctx, input.Sku, storeID, customerGroupID)
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

		var addErr error
		switch {
		case product.ProductType == "configurable" && len(input.SelectedOptions) > 0:
			addErr = s.addConfigurableProduct(ctx, quoteID, storeID, customerGroupID, product, input)
		case product.ProductType == "grouped":
			addErr = fmt.Errorf("Please specify the quantity of product(s).")
		case product.ProductType == "bundle" && len(input.SelectedOptions) > 0:
			addErr = s.addBundleProduct(ctx, quoteID, storeID, customerGroupID, product, input)
		default:
			_, addErr = s.itemRepo.Add(ctx, quoteID, product.ProductID, input.Sku, product.Name, product.ProductType, input.Quantity, product.Price)
		}
		if addErr != nil {
			userErrors = append(userErrors, &model.CartUserInputError{
				Code:    model.CartUserInputErrorTypeUndefined,
				Message: addErr.Error(),
			})
		}
	}

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}

	updatedCart, _ := s.GetCart(ctx, maskedID)
	return &model.AddProductsToCartOutput{Cart: updatedCart, UserErrors: userErrors}, nil
}

func (s *CartService) UpdateItems(ctx context.Context, maskedID string, items []*model.CartItemUpdateInput) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	for _, item := range items {
		itemID, _ := cartmapper.DecodeUID(item.CartItemUID)
		if item.Quantity <= 0 {
			s.itemRepo.Remove(ctx, itemID)
		} else {
			s.itemRepo.UpdateQty(ctx, itemID, item.Quantity)
		}
	}

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
		return nil, err
	}
	return s.GetCart(ctx, maskedID)
}

func (s *CartService) RemoveItem(ctx context.Context, maskedID string, itemUID string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}
	_ = quoteID

	itemID, _ := cartmapper.DecodeUID(itemUID)
	s.itemRepo.Remove(ctx, itemID)

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
		return nil, err
	}
	return s.GetCart(ctx, maskedID)
}

// ── Address operations ────────────────────────────────────────────────────────

func (s *CartService) SetShippingAddresses(ctx context.Context, maskedID string, addresses []*model.ShippingAddressInput) (*model.Cart, error) {
	if len(addresses) > 1 {
		return nil, carterr.ErrMultipleShippingAddresses
	}

	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	if err := s.checkGuestCheckoutAllowance(ctx, quoteID); err != nil {
		return nil, err
	}

	for _, addr := range addresses {
		if addr.CustomerAddressID != nil {
			customerID := middleware.GetCustomerID(ctx)
			if customerID == 0 {
				return nil, mgerrors.ErrUnauthorized
			}
			ca, err := s.loadCustomerAddress(ctx, *addr.CustomerAddressID, customerID)
			if err != nil {
				return nil, err
			}
			if _, err := s.addressRepo.SetAddress(ctx, quoteID, "shipping",
				ca.Firstname, ca.Lastname, ca.City, ca.CountryID, ca.Street,
				ca.Company, ca.Region, ca.Postcode, ca.Telephone, ca.RegionID,
			); err != nil {
				return nil, fmt.Errorf("failed to set shipping address: %w", err)
			}
			continue
		}
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

	// Persist rates to quote_shipping_rate — Magento writes these during
	// collectShippingRates() so that selected_shipping_method can read
	// carrier_title/method_title from storage later.
	cart, _ := s.cartRepo.GetByID(ctx, quoteID)
	items, _ := s.itemRepo.GetByQuoteID(ctx, quoteID)
	freshAddrs, _ := s.addressRepo.GetByQuoteID(ctx, quoteID)
	storeID := middleware.GetStoreID(ctx)
	for _, a := range freshAddrs {
		if a.AddressType == "shipping" {
			req := s.buildRateRequest(storeID, a, cart, items)
			rates := s.shippingRegistry.CollectRates(ctx, req)
			s.shippingRepo.SaveRates(ctx, a.AddressID, rates)
		}
	}

	return s.GetCart(ctx, maskedID)
}

func (s *CartService) SetBillingAddress(ctx context.Context, maskedID string, input *model.BillingAddressInput) (*model.Cart, error) {
	// Input mutual-exclusivity validation — mirrors Magento checkForInputExceptions
	sameAsShipping := input.SameAsShipping != nil && *input.SameAsShipping
	if !sameAsShipping && input.CustomerAddressID == nil && input.Address == nil {
		return nil, carterr.ErrBillingAddressInputMissing
	}
	if input.CustomerAddressID != nil && input.Address != nil {
		return nil, carterr.ErrBillingAddressInputConflict
	}

	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	if err := s.checkGuestCheckoutAllowance(ctx, quoteID); err != nil {
		return nil, err
	}

	switch {
	case sameAsShipping:
		// Validate shipping address exists and is set — mirrors validateCanUseShippingForBilling
		addrs, _ := s.addressRepo.GetByQuoteID(ctx, quoteID)
		var shippingAddrs []*repository.CartAddressData
		for _, a := range addrs {
			if a.AddressType == "shipping" {
				shippingAddrs = append(shippingAddrs, a)
			}
		}
		if len(shippingAddrs) > 1 {
			return nil, carterr.ErrSameAsShippingMultipleAddrs
		}
		if len(shippingAddrs) == 0 || shippingAddrs[0].CountryID == "" {
			return nil, carterr.ErrSameAsShippingNotSet
		}
		sa := shippingAddrs[0]
		street := strings.Split(sa.Street, "\n")
		if _, err := s.addressRepo.SetAddress(ctx, quoteID, "billing",
			sa.Firstname, sa.Lastname, sa.City, sa.CountryID, street,
			sa.Company, sa.Region, sa.Postcode, sa.Telephone, sa.RegionID,
		); err != nil {
			return nil, fmt.Errorf("failed to set billing address: %w", err)
		}
		// Magento sets same_as_billing=1 on the shipping address row
		s.addressRepo.SetSameAsBilling(ctx, quoteID, 1)

	case input.CustomerAddressID != nil:
		customerID := middleware.GetCustomerID(ctx)
		if customerID == 0 {
			return nil, mgerrors.ErrUnauthorized
		}
		ca, err := s.loadCustomerAddress(ctx, *input.CustomerAddressID, customerID)
		if err != nil {
			return nil, err
		}
		if _, err := s.addressRepo.SetAddress(ctx, quoteID, "billing",
			ca.Firstname, ca.Lastname, ca.City, ca.CountryID, ca.Street,
			ca.Company, ca.Region, ca.Postcode, ca.Telephone, ca.RegionID,
		); err != nil {
			return nil, fmt.Errorf("failed to set billing address: %w", err)
		}
		s.addressRepo.SetSameAsBilling(ctx, quoteID, 0)

	case input.Address != nil:
		a := input.Address
		if _, err := s.addressRepo.SetAddress(ctx, quoteID, "billing",
			a.Firstname, a.Lastname, a.City, a.CountryCode, a.Street,
			a.Company, a.Region, a.Postcode, a.Telephone, a.RegionID,
		); err != nil {
			return nil, fmt.Errorf("failed to set billing address: %w", err)
		}
		s.addressRepo.SetSameAsBilling(ctx, quoteID, 0)
	}

	return s.GetCart(ctx, maskedID)
}

// ── Shipping / payment ────────────────────────────────────────────────────────

func (s *CartService) SetShippingMethods(ctx context.Context, maskedID string, methods []*model.ShippingMethodInput) (*model.Cart, error) {
	if len(methods) > 1 {
		return nil, carterr.ErrMultipleShippingMethods
	}

	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	if err := s.checkGuestCheckoutAllowance(ctx, quoteID); err != nil {
		return nil, err
	}

	addrs, _ := s.addressRepo.GetByQuoteID(ctx, quoteID)
	cart, _ := s.cartRepo.GetByID(ctx, quoteID)
	items, _ := s.itemRepo.GetByQuoteID(ctx, quoteID)
	storeID := middleware.GetStoreID(ctx)

	for _, method := range methods {
		for _, a := range addrs {
			if a.AddressType != "shipping" {
				continue
			}
			req := s.buildRateRequest(storeID, a, cart, items)
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
			// Persist all collected rates to quote_shipping_rate so that
			// selected_shipping_method reads carrier_title/method_title from
			// storage — the same source Magento uses.
			s.shippingRepo.SaveRates(ctx, a.AddressID, rates)
			break
		}
	}

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}
	return s.GetCart(ctx, maskedID)
}

func (s *CartService) SetPaymentMethod(ctx context.Context, maskedID string, methodCode string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	if err := s.checkGuestCheckoutAllowance(ctx, quoteID); err != nil {
		return nil, err
	}

	storeID := middleware.GetStoreID(ctx)
	cart, _ := s.cartRepo.GetByID(ctx, quoteID)
	available := s.paymentRepo.GetAvailableMethods(ctx, storeID, cart.GrandTotal)
	if s.stripeClient.Configured() {
		available = append(available, &repository.PaymentMethod{Code: "stripe", Title: "Credit / Debit Card (Stripe)"})
	}
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

func (s *CartService) SetGuestEmail(ctx context.Context, maskedID, email string) (*model.Cart, error) {
	// Reject logged-in customers — matches Magento SetGuestEmailOnCart resolver
	if middleware.GetCustomerID(ctx) != 0 {
		return nil, carterr.ErrGuestEmailNotAllowed
	}

	// Validate email format
	if !isValidEmail(email) {
		return nil, carterr.ErrGuestEmailInvalid
	}

	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	// Check guest checkout allowance
	if err := s.checkGuestCheckoutAllowance(ctx, quoteID); err != nil {
		return nil, err
	}

	s.cartRepo.UpdateEmail(ctx, quoteID, email)
	return s.GetCart(ctx, maskedID)
}

// ── Shipping estimation ───────────────────────────────────────────────────────

func (s *CartService) EstimateShippingMethods(ctx context.Context, input model.EstimateShippingMethodsInput) ([]*model.AvailableShippingMethod, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, input.CartID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(input.CartID)
	}
	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(input.CartID)
	}

	items, _ := s.itemRepo.GetByQuoteID(ctx, quoteID)
	storeID := middleware.GetStoreID(ctx)
	tempAddr := &repository.CartAddressData{
		AddressType: "shipping",
		CountryID:   input.Address.CountryCode,
		RegionID:    input.Address.RegionID,
		Postcode:    input.Address.Postcode,
	}
	req := s.buildRateRequest(storeID, tempAddr, cart, items)
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

	tempAddr := &repository.CartAddressData{
		AddressType: "shipping",
		CountryID:   input.Address.CountryCode,
		RegionID:    input.Address.RegionID,
		Postcode:    input.Address.Postcode,
	}

	if input.ShippingMethod != nil {
		storeID := middleware.GetStoreID(ctx)
		req := s.buildRateRequest(storeID, tempAddr, cart, items)
		for _, r := range s.shippingRegistry.CollectRates(ctx, req) {
			if r.CarrierCode == input.ShippingMethod.CarrierCode && r.MethodCode == input.ShippingMethod.MethodCode {
				tempAddr.ShippingAmount = r.Price
				break
			}
		}
	}

	total, err := s.collectTotals(ctx, cart, items, []*repository.CartAddressData{tempAddr})
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

// ── Coupon operations ─────────────────────────────────────────────────────────

func (s *CartService) ApplyCoupon(ctx context.Context, maskedID, couponCode string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}
	if cart.CouponCode != nil && *cart.CouponCode != "" {
		return nil, fmt.Errorf("A coupon is already applied to the cart. Please remove it to apply another")
	}

	websiteID := s.cp.GetWebsiteID(cart.StoreID)
	customerGroupID := 0
	if cart.CustomerID != nil && *cart.CustomerID > 0 {
		customerGroupID = 1 // General
	}

	_, rule, err := s.couponRepo.LookupCoupon(ctx, couponCode, websiteID, customerGroupID)
	if err != nil {
		return nil, fmt.Errorf("The coupon code isn't valid. Verify the code and try again.")
	}

	items, _ := s.itemRepo.GetByQuoteID(ctx, quoteID)
	if len(items) == 0 {
		return nil, fmt.Errorf("Cart does not contain products.")
	}

	// Build cart eval context for conditions_serialized evaluation.
	var cartCtx repository.CartEvalContext
	for _, item := range items {
		if item.ParentItemID != nil {
			continue
		}
		cartCtx.Subtotal += item.RowTotal
		cartCtx.TotalQty += item.Qty
		cartCtx.TotalWeight += item.Weight * item.Qty
		cartCtx.Items = append(cartCtx.Items, repository.ItemEvalContext{
			SKU:         item.SKU,
			ProductType: item.ProductType,
			Price:       item.Price,
			CategoryIDs: s.couponRepo.GetItemCategoryIDs(ctx, item.ProductID),
		})
	}
	if addrs, addrErr := s.addressRepo.GetByQuoteID(ctx, quoteID); addrErr == nil {
		for _, addr := range addrs {
			if addr.AddressType == "shipping" {
				cartCtx.CountryID = addr.CountryID
				if addr.RegionID != nil {
					cartCtx.RegionID = *addr.RegionID
				}
				if addr.Region != nil {
					cartCtx.Region = *addr.Region
				}
				if addr.Postcode != nil {
					cartCtx.Postcode = *addr.Postcode
				}
				if addr.ShippingMethod != nil {
					cartCtx.ShippingMethod = *addr.ShippingMethod
				}
				break
			}
		}
	}

	condRoot := repository.ParseConditionTree(rule.ConditionsSerialized)
	if !repository.EvaluateCartConditions(condRoot, cartCtx) {
		return nil, fmt.Errorf("The coupon code isn't valid. Verify the code and try again.")
	}

	actRoot := repository.ParseConditionTree(rule.ActionsSerialized)
	ruleIDStr := fmt.Sprintf("%d", rule.RuleID)
	var totalDiscount float64
	type itemUpdate struct {
		itemID          int
		discountAmount  float64
		discountPercent float64
	}
	var updates []itemUpdate

	for _, item := range items {
		if item.ParentItemID != nil {
			continue
		}
		if !repository.EvaluateItemMatchesActions(actRoot, repository.ItemEvalContext{
			SKU:         item.SKU,
			ProductType: item.ProductType,
			Price:       item.Price,
			CategoryIDs: s.couponRepo.GetItemCategoryIDs(ctx, item.ProductID),
		}) {
			continue
		}

		var discountAmount, discountPercent float64
		switch rule.SimpleAction {
		case "by_percent":
			discountPercent = rule.DiscountAmount
			discountAmount = item.RowTotal * discountPercent / 100.0
		case "by_fixed":
			discountAmount = rule.DiscountAmount * item.Qty
		case "cart_fixed":
			if cartCtx.Subtotal > 0 {
				discountAmount = rule.DiscountAmount * (item.RowTotal / cartCtx.Subtotal)
			}
		case "to_percent":
			discountAmount = item.RowTotal * (1 - rule.DiscountAmount/100.0)
		case "to_fixed":
			discountAmount = math.Max(0, item.RowTotal-rule.DiscountAmount*item.Qty)
		case "buy_x_get_y":
			step := float64(rule.DiscountStep)
			free := rule.DiscountAmount
			if step > 0 && free > 0 {
				setSize := step + free
				sets := math.Floor(item.Qty / setSize)
				remainder := item.Qty - sets*setSize
				freeQty := sets*free + math.Max(0, remainder-step)
				discountAmount = item.Price * freeQty
			}
		}

		discountAmount = math.Round(discountAmount*100) / 100
		if discountAmount > item.RowTotal {
			discountAmount = item.RowTotal
		}
		totalDiscount += discountAmount
		updates = append(updates, itemUpdate{item.ItemID, discountAmount, discountPercent})
	}

	// Magento rejects the coupon when it would produce no discount for this cart
	// (e.g. action conditions target a SKU that is not in the cart).
	if totalDiscount == 0 {
		return nil, fmt.Errorf("The coupon code isn't valid. Verify the code and try again.")
	}

	for _, u := range updates {
		s.couponRepo.UpdateItemDiscount(ctx, u.itemID, u.discountAmount, u.discountPercent, ruleIDStr)
	}

	s.couponRepo.SetCouponOnQuote(ctx, quoteID, couponCode, ruleIDStr)

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}
	return s.GetCart(ctx, maskedID)
}

func (s *CartService) RemoveCoupon(ctx context.Context, maskedID string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}
	s.couponRepo.ClearCouponOnQuote(ctx, quoteID)
	s.couponRepo.ClearItemDiscounts(ctx, quoteID)
	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed")
	}
	return s.GetCart(ctx, maskedID)
}

// ── Cart merge / assign ───────────────────────────────────────────────────────

func (s *CartService) MergeCarts(ctx context.Context, sourceCartID string, destinationCartID *string) (*model.Cart, error) {
	customerID := middleware.GetCustomerID(ctx)
	if customerID == 0 {
		return nil, mgerrors.ErrUnauthorized
	}

	sourceQuoteID, err := s.maskRepo.Resolve(ctx, sourceCartID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(sourceCartID)
	}
	sourceCart, err := s.cartRepo.GetByID(ctx, sourceQuoteID)
	if err != nil || sourceCart.IsActive != 1 {
		return nil, carterr.ErrCartNotActive
	}

	var destMaskedID string
	var destQuoteID int
	if destinationCartID != nil && *destinationCartID != "" {
		destMaskedID = *destinationCartID
		destQuoteID, err = s.maskRepo.Resolve(ctx, destMaskedID)
		if err != nil {
			return nil, carterr.ErrCartNotFound(destMaskedID)
		}
	} else {
		storeID := middleware.GetStoreID(ctx)
		destCart, err := s.cartRepo.GetActiveByCustomerID(ctx, customerID, storeID)
		if err != nil {
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

	s.mergeItems(ctx, sourceQuoteID, destQuoteID)
	s.cartRepo.DeactivateSimple(ctx, sourceQuoteID)

	if err := s.recalculateTotals(ctx, destQuoteID); err != nil {
		log.Error().Err(err).Int("quote_id", destQuoteID).Msg("totals recalculation failed after merge")
	}
	return s.GetCart(ctx, destMaskedID)
}

func (s *CartService) AssignCustomerToGuestCart(ctx context.Context, guestCartID string) (*model.Cart, error) {
	customerID := middleware.GetCustomerID(ctx)
	if customerID == 0 {
		return nil, mgerrors.ErrUnauthorized
	}

	guestQuoteID, err := s.maskRepo.Resolve(ctx, guestCartID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(guestCartID)
	}
	guestCart, err := s.cartRepo.GetByID(ctx, guestQuoteID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(guestCartID)
	}
	if guestCart.CustomerID != nil && *guestCart.CustomerID > 0 {
		return nil, carterr.ErrCartForbidden(guestCartID)
	}

	storeID := middleware.GetStoreID(ctx)
	if oldCart, err := s.cartRepo.GetActiveByCustomerID(ctx, customerID, storeID); err == nil {
		s.mergeItems(ctx, oldCart.EntityID, guestQuoteID)
		s.cartRepo.DeactivateSimple(ctx, oldCart.EntityID)
	}

	s.cartRepo.SetCustomer(ctx, guestQuoteID, customerID)
	if err := s.recalculateTotals(ctx, guestQuoteID); err != nil {
		log.Error().Err(err).Int("quote_id", guestQuoteID).Msg("totals recalculation failed after assign")
	}

	maskedID, err := s.maskRepo.GetMaskedID(ctx, guestQuoteID)
	if err != nil {
		return nil, err
	}
	return s.GetCart(ctx, maskedID)
}

// ── Order placement ───────────────────────────────────────────────────────────

func (s *CartService) PlaceOrder(ctx context.Context, maskedID string) (*model.PlaceOrderOutput, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return orderErr(model.PlaceOrderErrorCodesCartNotFound, carterr.ErrCartNotFound(maskedID).Error()), nil
	}

	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return orderErr(model.PlaceOrderErrorCodesCartNotFound, carterr.ErrCartNotFound(maskedID).Error()), nil
	}
	if cart.IsActive != 1 {
		return orderErr(model.PlaceOrderErrorCodesCartNotActive, carterr.ErrCartNotActive.Error()), nil
	}

	// Mirrors GetCartForCheckout: check guest checkout allowance before placement
	if err := s.checkGuestCheckoutAllowance(ctx, quoteID); err != nil {
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, err.Error()), nil
	}

	items, err := s.itemRepo.GetByQuoteID(ctx, quoteID)
	if err != nil {
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrPlaceOrderFailed.Error()), nil
	}

	addrs, err := s.addressRepo.GetByQuoteID(ctx, quoteID)
	if err != nil {
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrPlaceOrderFailed.Error()), nil
	}

	// Auto-copy shipping → billing when no explicit billing address was set.
	// This mirrors Magento's behaviour when the checkout uses same_as_shipping.
	addrs = s.ensureBillingAddress(ctx, quoteID, addrs)

	payment, _ := s.paymentRepo.GetSelectedMethod(ctx, quoteID)

	if result := validateForOrder(cart, items, addrs, payment, middleware.GetCustomerID(ctx)); result != nil {
		return result, nil
	}

	// Task 94: re-verify stock before committing the order
	if stockErrors := s.checkStockForPlacement(ctx, items); len(stockErrors) > 0 {
		return &model.PlaceOrderOutput{Errors: stockErrors}, nil
	}

	// Task 97: refresh item prices from catalog index; recollect items if any changed
	if s.refreshItemPrices(ctx, cart, items) {
		if refreshed, err := s.itemRepo.GetByQuoteID(ctx, quoteID); err == nil {
			items = refreshed
		}
	}

	orderTotals, err := s.collectTotals(ctx, cart, items, addrs)
	if err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals collection for order failed")
	}

	// Populate per-item tax amounts from the pipeline so CartToOrder produces
	// correct price_incl_tax and row_total_incl_tax on order items.
	// Skip for inclusive-price stores: the price already contains tax.
	if orderTotals != nil && !orderTotals.TaxIncludedInPrice {
		for _, item := range items {
			if item.ParentItemID == nil {
				item.TaxAmount = orderTotals.ItemTaxes[item.ItemID]
			}
		}
	}

	orderIn := order.CartToOrder(cart, items, addrs, payment.Code, orderTotals)
	// Attach product_options JSON to each order item
	for i := range orderIn.Items {
		orderIn.Items[i].ProductOptions = repository.BuildProductOptionsJSON(ctx, s.orderRepo.DB(), items[i], items)
	}
	orderIn.RemoteIP = ctxkeys.GetRemoteIP(ctx)

	incrementID, protectCode, err := order.Place(ctx, s.orderRepo.DB(), orderIn)
	if err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("place order failed")
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrPlaceOrderFailed.Error()), nil
	}

	log.Info().Str("increment_id", incrementID).Int("quote_id", quoteID).Msg("order placed")
	return &model.PlaceOrderOutput{
		Errors:  []*model.PlaceOrderError{},
		OrderV2: &model.PlacedOrder{Number: incrementID, Token: protectCode},
	}, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// buildCart gathers all display data and delegates to the mapper.
func (s *CartService) buildCart(ctx context.Context, cart *repository.CartData, maskedID string) (*model.Cart, error) {
	items, _ := s.itemRepo.GetByQuoteID(ctx, cart.EntityID)
	addrs, _ := s.addressRepo.GetByQuoteID(ctx, cart.EntityID)
	displayTotals, _ := s.collectTotals(ctx, cart, items, addrs)

	storeID := middleware.GetStoreID(ctx)

	// Pre-fetch shipping rates per address (fresh, for available_shipping_methods).
	// Also load the persisted selected rate from quote_shipping_rate (for
	// selected_shipping_method carrier_title/method_title — same source Magento uses).
	shippingRates := make(map[int][]*shipping.Rate)
	storedRates := make(map[int]*repository.StoredShippingRate)
	for _, a := range addrs {
		if a.AddressType == "shipping" {
			req := s.buildRateRequest(storeID, a, cart, items)
			shippingRates[a.AddressID] = s.shippingRegistry.CollectRates(ctx, req)
			if a.ShippingMethod != nil && *a.ShippingMethod != "" {
				storedRates[a.AddressID] = s.shippingRepo.LoadRateByCode(ctx, a.AddressID, *a.ShippingMethod)
			}
		}
	}

	availPayments := s.paymentRepo.GetAvailableMethods(ctx, storeID, cart.GrandTotal)
	if s.stripeClient.Configured() {
		availPayments = append(availPayments, &repository.PaymentMethod{Code: "stripe", Title: "Credit / Debit Card (Stripe)"})
	}
	selectedPayment, _ := s.paymentRepo.GetSelectedMethod(ctx, cart.EntityID)

	// Build media base URL for product thumbnails.
	mediaBaseURL := strings.TrimRight(s.cp.Get("web/secure/base_media_url", storeID), "/")
	if mediaBaseURL == "" {
		baseURL := strings.TrimRight(s.cp.Get("web/secure/base_url", storeID), "/")
		if baseURL == "" {
			baseURL = "http://localhost"
		}
		mediaBaseURL = baseURL + "/media"
	}
	mediaBaseURL += "/catalog/product"

	return s.mapper.MapCart(ctx, cartmapper.MapCartInput{
		Cart:            cart,
		Items:           items,
		Addrs:           addrs,
		DisplayTotals:   displayTotals,
		ShippingRates:   shippingRates,
		StoredRates:     storedRates,
		AvailPayments:   availPayments,
		SelectedPayment: selectedPayment,
		MaskedID:        maskedID,
		MediaBaseURL:    mediaBaseURL,
	}), nil
}

// recalculateTotals recomputes and persists cart totals with a SELECT FOR UPDATE
// lock to prevent concurrent writes from producing inconsistent totals.
func (s *CartService) recalculateTotals(ctx context.Context, quoteID int) error {
	tx, cart, err := s.cartRepo.BeginTotalsUpdate(ctx, quoteID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	items, err := s.itemRepo.GetByQuoteID(ctx, quoteID)
	if err != nil {
		return err
	}
	addrs, _ := s.addressRepo.GetByQuoteID(ctx, quoteID)

	total, err := s.collectTotals(ctx, cart, items, addrs)
	if err != nil {
		return fmt.Errorf("totals pipeline: %w", err)
	}

	var itemsQty float64
	isVirtual := true
	for _, item := range items {
		if item.ParentItemID == nil {
			itemsQty += item.Qty
			if item.ProductType != "virtual" && item.ProductType != "downloadable" {
				isVirtual = false
			}
		}
	}
	if len(items) == 0 {
		isVirtual = false
	}

	if err := s.cartRepo.UpdateTotalsTx(ctx, tx, quoteID, total.Subtotal, total.GrandTotal, total.DiscountAmount, len(items), itemsQty, isVirtual); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CartService) collectTotals(ctx context.Context, cart *repository.CartData, items []*repository.CartItemData, addrs []*repository.CartAddressData) (*totals.Total, error) {
	var shippingAddr *repository.CartAddressData
	for _, a := range addrs {
		if a.AddressType == "shipping" {
			shippingAddr = a
			break
		}
	}
	cc := &totals.CollectorContext{
		Quote:              cart,
		Items:              items,
		Address:            shippingAddr,
		StoreID:            cart.StoreID,
		CustomerTaxClassID: s.taxRepo.GetCustomerTaxClassID(ctx, cart.CustomerGroupID),
	}
	return s.pipeline.Collect(ctx, cc)
}

func (s *CartService) buildRateRequest(storeID int, addr *repository.CartAddressData, cart *repository.CartData, items []*repository.CartItemData) *shipping.RateRequest {
	subtotalWithDiscount := cart.SubtotalWithDiscount
	if subtotalWithDiscount <= 0 {
		subtotalWithDiscount = cart.Subtotal
	}
	var totalWeight float64
	for _, item := range items {
		if item.ParentItemID == nil {
			totalWeight += item.Weight * item.Qty
		}
	}
	return &shipping.RateRequest{
		StoreID:              storeID,
		WebsiteID:            s.cp.GetWebsiteID(storeID),
		CountryID:            addr.CountryID,
		RegionID:             addr.RegionID,
		Postcode:             addr.Postcode,
		Subtotal:             cart.Subtotal,
		SubtotalWithDiscount: subtotalWithDiscount,
		ItemQty:              cart.ItemsQty,
		Weight:               totalWeight,
	}
}

// mergeItems copies all items from sourceQuoteID into destQuoteID (summing qty for same SKU).
func (s *CartService) mergeItems(ctx context.Context, sourceQuoteID, destQuoteID int) {
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
}

// ── Product operations ────────────────────────────────────────────────────────

type productInfo struct {
	ProductID   int
	ProductType string
	Name        string
	Price       float64
	Status      int
	StockStatus int
}

func (s *CartService) lookupProduct(ctx context.Context, sku string, storeID, customerGroupID int) (*productInfo, error) {
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
			ON cpe.entity_id = cpip.entity_id AND cpip.customer_group_id = ?
			AND cpip.website_id = (SELECT website_id FROM store WHERE store_id = ? LIMIT 1)
		LEFT JOIN catalog_product_entity_int cpei_status
			ON cpe.entity_id = cpei_status.entity_id
			AND cpei_status.attribute_id = (SELECT attribute_id FROM eav_attribute WHERE attribute_code = 'status' AND entity_type_id = 4)
			AND cpei_status.store_id = 0
		LEFT JOIN cataloginventory_stock_item csi ON cpe.entity_id = csi.product_id
		WHERE cpe.sku = ?`,
		customerGroupID, storeID, sku,
	).Scan(&p.ProductID, &p.ProductType, &p.Name, &p.Price, &p.Status, &p.StockStatus)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *CartService) addConfigurableProduct(ctx context.Context, quoteID, storeID, customerGroupID int, parent *productInfo, input *model.CartItemInput) error {
	db := s.cartRepo.DB()

	superAttributes := make(map[int]int)
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

	child, err := s.lookupProduct(ctx, matchedChild.sku, storeID, customerGroupID)
	if err != nil {
		return fmt.Errorf("Could not find product \"%s\"", matchedChild.sku)
	}
	if child.StockStatus != 1 {
		return fmt.Errorf("Product \"%s\" is out of stock.", input.Sku)
	}

	parentItemID, err := s.itemRepo.AddConfigurable(ctx, quoteID, parent.ProductID, matchedChild.sku, parent.Name, "configurable", input.Quantity, child.Price)
	if err != nil {
		return fmt.Errorf("Could not add \"%s\" to cart: %v", input.Sku, err)
	}
	_, err = s.itemRepo.AddChild(ctx, quoteID, child.ProductID, matchedChild.sku, child.Name, "simple", input.Quantity, parentItemID)
	if err != nil {
		return fmt.Errorf("Could not add \"%s\" to cart: %v", input.Sku, err)
	}
	// Store selected options so product_options can be built at order placement time
	attrsJSON, _ := jsonMarshalAttrs(superAttributes)
	buyRequestJSON := buildBuyRequestJSON(input.Quantity, superAttributes)
	s.itemRepo.WriteItemOption(ctx, parentItemID, parent.ProductID, "attributes", attrsJSON)
	s.itemRepo.WriteItemOption(ctx, parentItemID, parent.ProductID, "info_buyRequest", buyRequestJSON)
	s.itemRepo.WriteItemOption(ctx, parentItemID, parent.ProductID, "simple_product", strconv.Itoa(matchedChild.productID))
	return nil
}

func (s *CartService) addBundleProduct(ctx context.Context, quoteID, storeID, customerGroupID int, parent *productInfo, input *model.CartItemInput) error {
	db := s.cartRepo.DB()

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
			continue
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

	type childInfo struct {
		productID int
		sku, name string
		price, qty float64
	}
	var children []childInfo
	var totalPrice float64
	var childSkus []string

	for _, sel := range selections {
		var productID int
		var sku string
		var selPriceValue float64
		var selPriceType int // 0=fixed, 1=percent
		err := db.QueryRowContext(ctx,
			`SELECT bs.product_id,
			        (SELECT sku FROM catalog_product_entity WHERE entity_id = bs.product_id),
			        COALESCE(bs.selection_price_value, 0),
			        COALESCE(bs.selection_price_type, 0)
			 FROM catalog_product_bundle_selection bs
			 WHERE bs.selection_id = ? AND bs.parent_product_id = ?`,
			sel.selectionID, parent.ProductID,
		).Scan(&productID, &sku, &selPriceValue, &selPriceType)
		if err != nil {
			return fmt.Errorf("Invalid bundle selection %d", sel.selectionID)
		}

		childProduct, err := s.lookupProduct(ctx, sku, storeID, customerGroupID)
		if err != nil {
			return fmt.Errorf("Could not find product \"%s\"", sku)
		}

		// Use selection_price_value for pricing (Magento bundle pricing pattern).
		// type 0 = fixed price, type 1 = percent of parent price.
		var selPrice float64
		if selPriceType == 1 {
			selPrice = parent.Price * selPriceValue / 100.0
		} else {
			selPrice = selPriceValue
		}

		totalPrice += selPrice * sel.qty
		childSkus = append(childSkus, sku)
		children = append(children, childInfo{
			productID: productID,
			sku:       sku,
			name:      childProduct.Name,
			price:     selPrice,
			qty:       sel.qty,
		})
	}

	compositeSKU := input.Sku
	for _, sku := range childSkus {
		compositeSKU += "-" + sku
	}

	parentItemID, err := s.itemRepo.AddConfigurable(ctx, quoteID, parent.ProductID, compositeSKU, parent.Name, "bundle", input.Quantity, totalPrice)
	if err != nil {
		return fmt.Errorf("Could not add \"%s\" to cart: %v", input.Sku, err)
	}
	for _, child := range children {
		s.itemRepo.AddChild(ctx, quoteID, child.productID, child.sku, child.name, "simple", child.qty*input.Quantity, parentItemID)
	}
	return nil
}


// ── Customer address lookup ───────────────────────────────────────────────────

type customerAddrFields struct {
	Firstname, Lastname, City, CountryID string
	Street                               []string
	Company, Region, Postcode, Telephone *string
	RegionID                             *int
}

// loadCustomerAddress fetches a saved address from customer_address_entity.
// customerID is used for ownership verification.
func (s *CartService) loadCustomerAddress(ctx context.Context, addressID, customerID int) (*customerAddrFields, error) {
	var companyVal, regionVal, postcodeVal, telephoneVal sql.NullString
	var regionIDVal sql.NullInt32
	var streetRaw string
	var f customerAddrFields

	err := s.cartRepo.DB().QueryRowContext(ctx, `
		SELECT firstname, lastname, company, street, city, region, region_id, postcode, country_id, telephone
		FROM customer_address_entity
		WHERE entity_id = ? AND parent_id = ? AND is_active = 1`,
		addressID, customerID,
	).Scan(
		&f.Firstname, &f.Lastname, &companyVal, &streetRaw,
		&f.City, &regionVal, &regionIDVal, &postcodeVal,
		&f.CountryID, &telephoneVal,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("Address %d is not found.", addressID)
	}
	if err != nil {
		return nil, fmt.Errorf("load customer address: %w", err)
	}

	f.Street = strings.Split(streetRaw, "\n")
	if companyVal.Valid {
		f.Company = &companyVal.String
	}
	if regionVal.Valid {
		f.Region = &regionVal.String
	}
	if postcodeVal.Valid {
		f.Postcode = &postcodeVal.String
	}
	if telephoneVal.Valid {
		f.Telephone = &telephoneVal.String
	}
	if regionIDVal.Valid {
		v := int(regionIDVal.Int32)
		f.RegionID = &v
	}
	return &f, nil
}

// ── ReorderItems ──────────────────────────────────────────────────────────────

// ReorderItems re-adds top-level items from a previous order to the customer's active cart.
func (s *CartService) ReorderItems(ctx context.Context, orderNumber string) (*model.ReorderItemsOutput, error) {
	customerID := middleware.GetCustomerID(ctx)
	if customerID == 0 {
		return nil, mgerrors.ErrUnauthorized
	}

	db := s.orderRepo.DB()
	storeID := middleware.GetStoreID(ctx)

	// 1. Verify order belongs to customer
	var orderID int
	err := db.QueryRowContext(ctx,
		"SELECT entity_id FROM sales_order WHERE increment_id = ? AND customer_id = ?",
		orderNumber, customerID,
	).Scan(&orderID)
	if err != nil {
		return nil, fmt.Errorf("Cannot find order %s.", orderNumber)
	}

	// 2. Load top-level order items
	rows, err := db.QueryContext(ctx, `
		SELECT sku, product_type, qty_ordered, COALESCE(product_options, '')
		FROM sales_order_item
		WHERE order_id = ? AND parent_item_id IS NULL`,
		orderID)
	if err != nil {
		return nil, fmt.Errorf("Cannot load items for order %s.", orderNumber)
	}
	defer rows.Close()

	type orderItemRow struct {
		sku, productType, productOptions string
		qty                              float64
	}
	var orderItems []orderItemRow
	for rows.Next() {
		var item orderItemRow
		rows.Scan(&item.sku, &item.productType, &item.qty, &item.productOptions)
		orderItems = append(orderItems, item)
	}
	rows.Close()

	// 3. Ensure customer has an active cart
	var quoteID int
	activeCart, err := s.cartRepo.GetActiveByCustomerID(ctx, customerID, storeID)
	if err != nil {
		quoteID, err = s.cartRepo.Create(ctx, storeID, &customerID)
		if err != nil {
			return nil, err
		}
	} else {
		quoteID = activeCart.EntityID
	}
	maskedID, err := s.maskRepo.GetMaskedID(ctx, quoteID)
	if err != nil {
		return nil, err
	}
	cartData, _ := s.cartRepo.GetByID(ctx, quoteID)
	customerGroupID := 1
	if cartData != nil {
		customerGroupID = cartData.CustomerGroupID
	}

	// 4. Add each item, collecting per-item errors
	var userErrors []*model.CheckoutUserInputError
	for _, item := range orderItems {
		input := &model.CartItemInput{
			Sku:             item.sku,
			Quantity:        item.qty,
			SelectedOptions: reorderSelectedOptions(item.productType, item.productOptions),
		}

		product, err := s.lookupProduct(ctx, item.sku, storeID, customerGroupID)
		if err != nil {
			userErrors = append(userErrors, reorderInputErr(model.CheckoutUserInputErrorCodesProductNotFound, carterr.ErrProductNotFound(item.sku).Error(), item.sku))
			continue
		}
		if product.Status != 1 {
			userErrors = append(userErrors, reorderInputErr(model.CheckoutUserInputErrorCodesNotSalable, carterr.ErrNotSalable(item.sku).Error(), item.sku))
			continue
		}
		if product.StockStatus != 1 {
			userErrors = append(userErrors, reorderInputErr(model.CheckoutUserInputErrorCodesInsufficientStock, carterr.ErrOutOfStock(item.sku).Error(), item.sku))
			continue
		}

		var addErr error
		switch {
		case product.ProductType == "configurable" && len(input.SelectedOptions) > 0:
			addErr = s.addConfigurableProduct(ctx, quoteID, storeID, customerGroupID, product, input)
		case product.ProductType == "bundle" && len(input.SelectedOptions) > 0:
			addErr = s.addBundleProduct(ctx, quoteID, storeID, customerGroupID, product, input)
		default:
			_, addErr = s.itemRepo.Add(ctx, quoteID, product.ProductID, input.Sku, product.Name, product.ProductType, input.Quantity, product.Price)
		}
		if addErr != nil {
			userErrors = append(userErrors, reorderInputErr(model.CheckoutUserInputErrorCodesUndefined, addErr.Error(), item.sku))
		}
	}

	if err := s.recalculateTotals(ctx, quoteID); err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("totals recalculation failed after reorder")
	}

	cartModel, _ := s.GetCart(ctx, maskedID)
	if userErrors == nil {
		userErrors = []*model.CheckoutUserInputError{}
	}
	return &model.ReorderItemsOutput{Cart: cartModel, UserInputErrors: userErrors}, nil
}

// reorderSelectedOptions reconstructs SelectedOptions UIDs from a sales_order_item
// product_options JSON. Returns nil for simple products or unparseable JSON.
func reorderSelectedOptions(productType, productOptionsJSON string) []string {
	if productOptionsJSON == "" || (productType != "configurable" && productType != "bundle") {
		return nil
	}
	var opts struct {
		InfoBuyRequest struct {
			SuperAttribute  map[string]json.RawMessage `json:"super_attribute"`
			BundleOption    map[string]json.RawMessage `json:"bundle_option"`
			BundleOptionQty map[string]json.RawMessage `json:"bundle_option_qty"`
		} `json:"info_buyRequest"`
	}
	if err := json.Unmarshal([]byte(productOptionsJSON), &opts); err != nil {
		return nil
	}

	rawString := func(r json.RawMessage) string {
		s := strings.Trim(string(r), `"`)
		return s
	}

	var selected []string
	switch productType {
	case "configurable":
		for attrIDStr, optIDRaw := range opts.InfoBuyRequest.SuperAttribute {
			optIDStr := rawString(optIDRaw)
			uid := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("configurable/%s/%s", attrIDStr, optIDStr)))
			selected = append(selected, uid)
		}
	case "bundle":
		for optIDStr, selIDRaw := range opts.InfoBuyRequest.BundleOption {
			selIDStr := rawString(selIDRaw)
			qty := 1.0
			if qtyRaw, ok := opts.InfoBuyRequest.BundleOptionQty[optIDStr]; ok {
				if q, err := strconv.ParseFloat(rawString(qtyRaw), 64); err == nil {
					qty = q
				}
			}
			uid := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("bundle/%s/%s/%v", optIDStr, selIDStr, qty)))
			selected = append(selected, uid)
		}
	}
	return selected
}

func reorderInputErr(code model.CheckoutUserInputErrorCodes, message, sku string) *model.CheckoutUserInputError {
	return &model.CheckoutUserInputError{Code: code, Message: message, Path: []string{sku}}
}

// jsonMarshalAttrs encodes a map[attrID]optionID as a JSON string map ({"142":"166",...}).
func jsonMarshalAttrs(superAttributes map[int]int) (string, error) {
	m := make(map[string]string, len(superAttributes))
	for k, v := range superAttributes {
		m[strconv.Itoa(k)] = strconv.Itoa(v)
	}
	b, err := json.Marshal(m)
	return string(b), err
}

// checkGuestCheckoutAllowance mirrors Magento's CheckCartCheckoutAllowance.
// Returns an error if the cart is a guest cart AND guest checkout is disabled.
func (s *CartService) checkGuestCheckoutAllowance(ctx context.Context, quoteID int) error {
	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil // can't determine; let it pass
	}
	// Only guest carts need the check
	if cart.CustomerID != nil && *cart.CustomerID > 0 {
		return nil
	}
	storeID := middleware.GetStoreID(ctx)
	// Default is 1 (enabled) — only reject if explicitly disabled
	if s.cp.GetInt("checkout/options/guest_checkout", storeID, 1) == 0 {
		return carterr.ErrGuestCheckoutNotAllowed
	}
	return nil
}

// isValidEmail performs basic RFC 5322-style email validation.
func isValidEmail(email string) bool {
	at := strings.Index(email, "@")
	if at < 1 {
		return false
	}
	local := email[:at]
	domain := email[at+1:]
	if len(local) == 0 || len(domain) < 3 {
		return false
	}
	dot := strings.LastIndex(domain, ".")
	return dot > 0 && dot < len(domain)-1
}

// buildBuyRequestJSON builds the info_buyRequest JSON for a configurable item.
func buildBuyRequestJSON(qty float64, superAttributes map[int]int) string {
	superAttr := make(map[string]string, len(superAttributes))
	for k, v := range superAttributes {
		superAttr[strconv.Itoa(k)] = strconv.Itoa(v)
	}
	type buyReq struct {
		Qty            float64           `json:"qty"`
		SuperAttribute map[string]string `json:"super_attribute"`
		Options        []interface{}     `json:"options"`
	}
	b, _ := json.Marshal(buyReq{
		Qty:            qty,
		SuperAttribute: superAttr,
		Options:        []interface{}{},
	})
	return string(b)
}

// checkStockForPlacement re-verifies available inventory for all top-level cart
// items before order placement. Returns PlaceOrderErrors for any item with
// insufficient stock. Configurable children are skipped — stock is tracked on
// the simple child SKU but reserved against the parent in inventory_reservation.
func (s *CartService) checkStockForPlacement(ctx context.Context, items []*repository.CartItemData) []*model.PlaceOrderError {
	var topItems []*repository.CartItemData
	for _, item := range items {
		if item.ParentItemID == nil {
			topItems = append(topItems, item)
		}
	}
	if len(topItems) == 0 {
		return nil
	}

	args := make([]interface{}, len(topItems))
	for i, item := range topItems {
		args[i] = item.ProductID
	}
	ph := strings.Repeat("?,", len(args)-1) + "?"

	rows, err := s.cartRepo.DB().QueryContext(ctx, fmt.Sprintf(`
		SELECT csi.product_id, cpe.sku, csi.manage_stock, csi.is_in_stock,
		       COALESCE(csi.qty, 0) + COALESCE(ir_sum.qty_reserved, 0) AS available_qty
		FROM cataloginventory_stock_item csi
		JOIN catalog_product_entity cpe ON cpe.entity_id = csi.product_id
		LEFT JOIN (
		    SELECT sku, SUM(quantity) AS qty_reserved
		    FROM inventory_reservation
		    WHERE stock_id = 1
		    GROUP BY sku
		) ir_sum ON ir_sum.sku = cpe.sku
		WHERE csi.product_id IN (%s)`, ph), args...)
	if err != nil {
		log.Error().Err(err).Msg("pre-placement stock check failed")
		return nil
	}
	defer rows.Close()

	type stockRow struct {
		productID    int
		sku          string
		manageStock  int
		isInStock    int
		availableQty float64
	}
	stock := make(map[int]stockRow, len(topItems))
	for rows.Next() {
		var r stockRow
		rows.Scan(&r.productID, &r.sku, &r.manageStock, &r.isInStock, &r.availableQty)
		stock[r.productID] = r
	}

	var errs []*model.PlaceOrderError
	for _, item := range topItems {
		sr, ok := stock[item.ProductID]
		if !ok {
			continue
		}
		if sr.isInStock == 0 {
			errs = append(errs, &model.PlaceOrderError{
				Code:    model.PlaceOrderErrorCodesInsufficientStock,
				Message: carterr.ErrInsufficientStock(item.SKU, item.Qty, 0).Error(),
			})
			continue
		}
		if sr.manageStock == 1 && sr.availableQty < item.Qty {
			errs = append(errs, &model.PlaceOrderError{
				Code:    model.PlaceOrderErrorCodesInsufficientStock,
				Message: carterr.ErrInsufficientStock(item.SKU, item.Qty, math.Max(0, sr.availableQty)).Error(),
			})
		}
	}
	return errs
}

// refreshItemPrices compares stored quote_item prices against the current
// catalog price index and updates any items whose price has changed by more
// than $0.01. Returns true if at least one item was updated so the caller
// can re-read items and recollect totals.
func (s *CartService) refreshItemPrices(ctx context.Context, cart *repository.CartData, items []*repository.CartItemData) bool {
	var topItems []*repository.CartItemData
	for _, item := range items {
		if item.ParentItemID == nil {
			topItems = append(topItems, item)
		}
	}
	if len(topItems) == 0 {
		return false
	}

	websiteID := s.cp.GetWebsiteID(cart.StoreID)
	ph := strings.Repeat("?,", len(topItems)-1) + "?"
	args := make([]interface{}, 0, 2+len(topItems))
	args = append(args, cart.CustomerGroupID, websiteID)
	for _, item := range topItems {
		args = append(args, item.ProductID)
	}

	rows, err := s.cartRepo.DB().QueryContext(ctx, fmt.Sprintf(`
		SELECT entity_id, COALESCE(final_price, 0)
		FROM catalog_product_index_price
		WHERE customer_group_id = ? AND website_id = ? AND entity_id IN (%s)`, ph), args...)
	if err != nil {
		log.Error().Err(err).Msg("price refresh query failed")
		return false
	}
	defer rows.Close()

	currentPrices := make(map[int]float64, len(topItems))
	for rows.Next() {
		var productID int
		var price float64
		rows.Scan(&productID, &price)
		currentPrices[productID] = price
	}

	updated := false
	for _, item := range topItems {
		newPrice, ok := currentPrices[item.ProductID]
		if !ok || newPrice <= 0 {
			continue
		}
		if math.Abs(newPrice-item.Price) > 0.01 {
			if err := s.itemRepo.UpdatePrice(ctx, item.ItemID, newPrice, item.Qty); err != nil {
				log.Error().Err(err).Int("item_id", item.ItemID).Msg("failed to update item price before placement")
				continue
			}
			log.Info().Str("sku", item.SKU).
				Float64("old_price", item.Price).Float64("new_price", newPrice).
				Msg("cart item price updated before placement")
			updated = true
		}
	}
	return updated
}

// ── Stripe Checkout ───────────────────────────────────────────────────────────

// CreateStripeCheckoutSession creates a Stripe Checkout Session for a cart.
// The cart must have items, a shipping address, and a shipping method set.
// On success returns the Stripe-hosted checkout URL and session ID.
func (s *CartService) CreateStripeCheckoutSession(ctx context.Context, maskedCartID, successURL, cancelURL string) (*model.CreateStripeCheckoutSessionOutput, error) {
	if !s.stripeClient.Configured() {
		return nil, fmt.Errorf("Stripe is not configured on this store.")
	}

	quoteID, err := s.maskRepo.Resolve(ctx, maskedCartID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedCartID)
	}

	cart, err := s.cartRepo.GetByID(ctx, quoteID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedCartID)
	}
	if cart.IsActive == 0 {
		return nil, carterr.ErrCartNotActive
	}

	items, err := s.itemRepo.GetByQuoteID(ctx, quoteID)
	if err != nil || len(items) == 0 {
		return nil, fmt.Errorf("Cart is empty.")
	}

	// Set stripe as the payment method on the cart
	_ = s.paymentRepo.SetPaymentMethod(ctx, quoteID, "stripe")

	// Build Stripe line items from top-level cart items only (no child rows)
	currency := strings.ToLower(cart.QuoteCurrencyCode)
	if currency == "" {
		currency = "usd"
	}

	var lineItems []payment.LineItem
	for _, item := range items {
		if item.ParentItemID != nil {
			continue // skip child items — the configurable parent carries the price
		}
		unitCents := int64(math.Round(item.Price * 100))
		if unitCents <= 0 {
			continue
		}
		lineItems = append(lineItems, payment.LineItem{
			Name:     item.Name,
			Amount:   unitCents,
			Currency: currency,
			Qty:      int64(math.Round(item.Qty)),
		})
	}

	// Add shipping as a line item if non-zero
	addrs, _ := s.addressRepo.GetByQuoteID(ctx, quoteID)
	for _, addr := range addrs {
		if addr.AddressType == "shipping" && addr.ShippingAmount > 0 {
			shippingCents := int64(math.Round(addr.ShippingAmount * 100))
			lineItems = append(lineItems, payment.LineItem{
				Name:     "Shipping",
				Amount:   shippingCents,
				Currency: currency,
				Qty:      1,
			})
			break
		}
	}

	if len(lineItems) == 0 {
		return nil, fmt.Errorf("Cart has no chargeable items.")
	}

	result, err := s.stripeClient.CreateCheckoutSession(ctx, maskedCartID, lineItems, successURL, cancelURL)
	if err != nil {
		return nil, fmt.Errorf("Unable to create Stripe Checkout Session: %w", err)
	}

	return &model.CreateStripeCheckoutSessionOutput{
		CheckoutURL: result.URL,
		SessionID:   result.SessionID,
	}, nil
}

// ensureBillingAddress copies the shipping address to billing when no billing
// address has been explicitly set. This mirrors Magento's same_as_shipping
// behaviour for storefronts that don't have a separate billing address step.
func (s *CartService) ensureBillingAddress(ctx context.Context, quoteID int, addrs []*repository.CartAddressData) []*repository.CartAddressData {
	var hasBilling, hasShipping bool
	var shippingAddr *repository.CartAddressData
	for _, a := range addrs {
		switch a.AddressType {
		case "billing":
			hasBilling = true
		case "shipping":
			hasShipping = true
			shippingAddr = a
		}
	}
	if hasBilling || !hasShipping || shippingAddr == nil {
		return addrs
	}

	// Build street slice from newline-separated string
	streetLines := strings.Split(shippingAddr.Street, "\n")
	_, err := s.addressRepo.SetAddress(
		ctx, quoteID, "billing",
		shippingAddr.Firstname, shippingAddr.Lastname, shippingAddr.City, shippingAddr.CountryID,
		streetLines,
		shippingAddr.Company, shippingAddr.Region, shippingAddr.Postcode, shippingAddr.Telephone,
		shippingAddr.RegionID,
	)
	if err != nil {
		log.Warn().Err(err).Int("quote_id", quoteID).Msg("failed to auto-copy shipping to billing")
		return addrs
	}

	// Append a synthetic billing address so the validator sees it without a DB round-trip
	billing := *shippingAddr
	billing.AddressType = "billing"
	return append(addrs, &billing)
}
