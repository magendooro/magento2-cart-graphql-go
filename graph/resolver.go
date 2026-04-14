package graph

import (
	"database/sql"
	"fmt"

	"github.com/magendooro/magento2-cart-graphql-go/internal/payment"
	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
	"github.com/magendooro/magento2-cart-graphql-go/internal/service"
	"github.com/magendooro/magento2-cart-graphql-go/internal/shipping"
	"github.com/magendooro/magento2-go-common/config"
	"github.com/magendooro/magento2-go-common/middleware"
)

type Resolver struct {
	CartService       *service.CartService
	AgreementRepo     *repository.CheckoutAgreementRepository
	ConfigProvider    *config.ConfigProvider
	TokenResolver     *middleware.TokenResolver
	StripeClient      *payment.StripeClient
}

func NewResolver(db *sql.DB, cp *config.ConfigProvider, stripeClient *payment.StripeClient) (*Resolver, error) {
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
	couponRepo := repository.NewCouponRepository(db)

	shippingRegistry := shipping.NewRegistry(
		&shipping.FlatrateCarrier{CP: cp},
		&shipping.TablerateCarrier{DB: db, CP: cp},
		&shipping.FreeshippingCarrier{CP: cp},
	)

	cartService := service.NewCartService(cartRepo, maskRepo, itemRepo, addressRepo, shippingRepo, shippingRegistry, paymentRepo, taxRepo, orderRepo, couponRepo, cp)
	agreementRepo := repository.NewCheckoutAgreementRepository(db)

	return &Resolver{
		CartService:    cartService,
		AgreementRepo:  agreementRepo,
		ConfigProvider: cp,
		StripeClient:   stripeClient,
	}, nil
}
