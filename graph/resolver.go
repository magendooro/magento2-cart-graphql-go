package graph

import (
	"database/sql"
	"fmt"

	"github.com/magendooro/magento2-cart-graphql-go/internal/config"
	"github.com/magendooro/magento2-cart-graphql-go/internal/middleware"
	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
	"github.com/magendooro/magento2-cart-graphql-go/internal/service"
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

	cartService := service.NewCartService(cartRepo, maskRepo, itemRepo, cp)

	return &Resolver{
		CartService: cartService,
	}, nil
}
