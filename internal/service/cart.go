package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/magendooro/magento2-cart-graphql-go/graph/model"
	"github.com/magendooro/magento2-cart-graphql-go/internal/config"
	"github.com/magendooro/magento2-cart-graphql-go/internal/middleware"
	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
)

type CartService struct {
	cartRepo     *repository.CartRepository
	maskRepo     *repository.CartMaskRepository
	itemRepo     *repository.CartItemRepository
	cp           *config.ConfigProvider
}

func NewCartService(
	cartRepo *repository.CartRepository,
	maskRepo *repository.CartMaskRepository,
	itemRepo *repository.CartItemRepository,
	cp *config.ConfigProvider,
) *CartService {
	return &CartService{
		cartRepo: cartRepo,
		maskRepo: maskRepo,
		itemRepo: itemRepo,
		cp:       cp,
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
	s.recalculateTotals(ctx, quoteID)

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

	s.recalculateTotals(ctx, quoteID)
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

	s.recalculateTotals(ctx, quoteID)
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

// recalculateTotals recomputes subtotal and updates the quote.
func (s *CartService) recalculateTotals(ctx context.Context, quoteID int) {
	items, err := s.itemRepo.GetByQuoteID(ctx, quoteID)
	if err != nil {
		return
	}

	var subtotal float64
	var itemsQty float64
	for _, item := range items {
		if item.ParentItemID == nil { // only count top-level items
			subtotal += item.RowTotal
			itemsQty += item.Qty
		}
	}

	// Phase 1: grand_total = subtotal (tax and shipping added in later phases)
	s.cartRepo.UpdateTotals(ctx, quoteID, subtotal, subtotal, len(items), itemsQty)
}

// ── Mapping ─────────────────────────────────────────────────────────────────

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

	result := &model.Cart{
		ID:            maskedID,
		Items:         cartItems,
		TotalQuantity: totalQty,
		IsVirtual:     isVirtual,
		Email:         cart.CustomerEmail,
		Prices: &model.CartPrices{
			GrandTotal:            &model.Money{Value: &cart.GrandTotal, Currency: &currency},
			SubtotalExcludingTax:  &model.Money{Value: &cart.Subtotal, Currency: &currency},
			SubtotalIncludingTax:  &model.Money{Value: &cart.Subtotal, Currency: &currency}, // Phase 1: no tax yet
		},
		ShippingAddresses: []*model.ShippingCartAddress{},
	}

	if cart.CouponCode != nil {
		result.AppliedCoupons = []*model.AppliedCoupon{{Code: *cart.CouponCode}}
	}

	return result, nil
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
