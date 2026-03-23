package shipping

import (
	"context"

	"github.com/magendooro/magento2-cart-graphql-go/internal/config"
)

// FreeshippingCarrier implements free shipping above a configurable threshold.
type FreeshippingCarrier struct {
	CP *config.ConfigProvider
}

func (c *FreeshippingCarrier) Code() string { return "freeshipping" }

func (c *FreeshippingCarrier) IsActive(storeID int) bool {
	return c.CP.GetBool("carriers/freeshipping/active", storeID)
}

func (c *FreeshippingCarrier) CollectRates(_ context.Context, req *RateRequest) ([]*Rate, error) {
	threshold := c.CP.GetFloat("carriers/freeshipping/free_shipping_subtotal", req.StoreID, 0)
	if threshold > 0 && req.Subtotal < threshold {
		return nil, nil // not eligible
	}

	return []*Rate{{
		CarrierCode:  "freeshipping",
		CarrierTitle: "Free Shipping",
		MethodCode:   "freeshipping",
		MethodTitle:  "Free",
		Price:        0,
	}}, nil
}
