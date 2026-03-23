package graph

import (
	"database/sql"
	"fmt"

	"github.com/magendooro/magento2-cart-graphql-go/internal/config"
	"github.com/magendooro/magento2-cart-graphql-go/internal/middleware"
	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
	"github.com/magendooro/magento2-cart-graphql-go/internal/service"
	"github.com/magendooro/magento2-cart-graphql-go/internal/shipping"
)

type Resolver struct {
	CartService   *service.CartService
	TokenResolver *middleware.TokenResolver
}

func NewResolver(db *sql.DB, cp *config.ConfigProvider) (*Resolver, error) {
	if cp == nil {
		return nil, fmt.Errorf("ConfigProvider is required")
	}

	cartRepo := repository.NewCartRepository(db)
	maskRepo := repository.NewCartMaskRepository(db)
	itemRepo := repository.NewCartItemRepository(db)
	addressRepo := repository.NewCartAddressRepository(db)
	shippingRepo := repository.NewShippingRepository(db, cp)
	paymentRepo := repository.NewPaymentRepository(db, cp)
	taxRepo := repository.NewTaxRepository(db)
	orderRepo := repository.NewOrderRepository(db)

	// Build carrier registry (Strategy pattern — each carrier is independent)
	shippingRegistry := shipping.NewRegistry(
		&shipping.FlatrateCarrier{CP: cp},
		&shipping.TablerateCarrier{DB: db, CP: cp},
		&shipping.FreeshippingCarrier{CP: cp},
	)

	cartService := service.NewCartService(cartRepo, maskRepo, itemRepo, addressRepo, shippingRepo, shippingRegistry, paymentRepo, taxRepo, orderRepo, cp)

	return &Resolver{
		CartService: cartService,
	}, nil
}
