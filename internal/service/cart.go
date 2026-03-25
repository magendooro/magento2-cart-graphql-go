package service

import (
	"context"
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
	}
	return s.GetCart(ctx, maskedID)
}

// ── Address operations ────────────────────────────────────────────────────────

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

func (s *CartService) SetBillingAddress(ctx context.Context, maskedID string, input *model.BillingAddressInput) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
	}

	if input.SameAsShipping != nil && *input.SameAsShipping {
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

// ── Shipping / payment ────────────────────────────────────────────────────────

func (s *CartService) SetShippingMethods(ctx context.Context, maskedID string, methods []*model.ShippingMethodInput) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
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

func (s *CartService) SetGuestEmail(ctx context.Context, maskedID, email string) (*model.Cart, error) {
	quoteID, err := s.maskRepo.Resolve(ctx, maskedID)
	if err != nil {
		return nil, carterr.ErrCartNotFound(maskedID)
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

		var discountAmount, discountPercent float64
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

	items, err := s.itemRepo.GetByQuoteID(ctx, quoteID)
	if err != nil {
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrPlaceOrderFailed.Error()), nil
	}

	addrs, err := s.addressRepo.GetByQuoteID(ctx, quoteID)
	if err != nil {
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrPlaceOrderFailed.Error()), nil
	}

	payment, _ := s.paymentRepo.GetSelectedMethod(ctx, quoteID)

	if result := validateForOrder(cart, items, addrs, payment); result != nil {
		return result, nil
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

	incrementID, err := order.Place(ctx, s.orderRepo.DB(), orderIn)
	if err != nil {
		log.Error().Err(err).Int("quote_id", quoteID).Msg("place order failed")
		return orderErr(model.PlaceOrderErrorCodesUnableToPlaceOrder, carterr.ErrPlaceOrderFailed.Error()), nil
	}

	log.Info().Str("increment_id", incrementID).Int("quote_id", quoteID).Msg("order placed")
	return &model.PlaceOrderOutput{
		Errors:  []*model.PlaceOrderError{},
		OrderV2: &model.PlacedOrder{Number: incrementID},
	}, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// buildCart gathers all display data and delegates to the mapper.
func (s *CartService) buildCart(ctx context.Context, cart *repository.CartData, maskedID string) (*model.Cart, error) {
	items, _ := s.itemRepo.GetByQuoteID(ctx, cart.EntityID)
	addrs, _ := s.addressRepo.GetByQuoteID(ctx, cart.EntityID)
	displayTotals, _ := s.collectTotals(ctx, cart, items, addrs)

	storeID := middleware.GetStoreID(ctx)

	// Pre-fetch shipping rates per address
	shippingRates := make(map[int][]*shipping.Rate)
	for _, a := range addrs {
		if a.AddressType == "shipping" {
			req := s.buildRateRequest(storeID, a, cart, items)
			shippingRates[a.AddressID] = s.shippingRegistry.CollectRates(ctx, req)
		}
	}

	availPayments := s.paymentRepo.GetAvailableMethods(ctx, storeID, cart.GrandTotal)
	selectedPayment, _ := s.paymentRepo.GetSelectedMethod(ctx, cart.EntityID)

	return s.mapper.MapCart(ctx, cartmapper.MapCartInput{
		Cart:            cart,
		Items:           items,
		Addrs:           addrs,
		DisplayTotals:   displayTotals,
		ShippingRates:   shippingRates,
		AvailPayments:   availPayments,
		SelectedPayment: selectedPayment,
		MaskedID:        maskedID,
	}), nil
}

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

	return s.cartRepo.UpdateTotals(ctx, quoteID, total.Subtotal, total.GrandTotal, total.DiscountAmount, len(items), itemsQty, isVirtual)
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
		err := db.QueryRowContext(ctx,
			"SELECT product_id, (SELECT sku FROM catalog_product_entity WHERE entity_id = bs.product_id) FROM catalog_product_bundle_selection bs WHERE bs.selection_id = ? AND bs.parent_product_id = ?",
			sel.selectionID, parent.ProductID,
		).Scan(&productID, &sku)
		if err != nil {
			return fmt.Errorf("Invalid bundle selection %d", sel.selectionID)
		}

		childProduct, err := s.lookupProduct(ctx, sku, storeID, customerGroupID)
		if err != nil {
			return fmt.Errorf("Could not find product \"%s\"", sku)
		}

		totalPrice += childProduct.Price * sel.qty
		childSkus = append(childSkus, sku)
		children = append(children, childInfo{
			productID: productID,
			sku:       sku,
			name:      childProduct.Name,
			price:     childProduct.Price,
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


// jsonMarshalAttrs encodes a map[attrID]optionID as a JSON string map ({"142":"166",...}).
func jsonMarshalAttrs(superAttributes map[int]int) (string, error) {
	m := make(map[string]string, len(superAttributes))
	for k, v := range superAttributes {
		m[strconv.Itoa(k)] = strconv.Itoa(v)
	}
	b, err := json.Marshal(m)
	return string(b), err
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
