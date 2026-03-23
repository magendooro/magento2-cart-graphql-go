package shipping

import (
	"context"

	"github.com/magendooro/magento2-cart-graphql-go/internal/config"
)

// FlatrateCarrier implements flat rate shipping.
// Default: active, $5/item (type=I), title "Flat Rate", method "Fixed".
type FlatrateCarrier struct {
	CP *config.ConfigProvider
}

func (c *FlatrateCarrier) Code() string { return "flatrate" }

func (c *FlatrateCarrier) IsActive(storeID int) bool {
	return c.CP.GetInt("carriers/flatrate/active", storeID, 1) == 1
}

func (c *FlatrateCarrier) CollectRates(_ context.Context, req *RateRequest) ([]*Rate, error) {
	unitPrice := c.CP.GetFloat("carriers/flatrate/price", req.StoreID, 5.0)

	// Magento default type is "I" (per-item). "O" = per-order.
	rateType := c.CP.Get("carriers/flatrate/type", req.StoreID)
	if rateType == "" {
		rateType = "I"
	}

	price := unitPrice
	if rateType == "I" && req.ItemQty > 0 {
		price = unitPrice * req.ItemQty
	}

	title := c.CP.Get("carriers/flatrate/title", req.StoreID)
	if title == "" {
		title = "Flat Rate"
	}
	methodTitle := c.CP.Get("carriers/flatrate/name", req.StoreID)
	if methodTitle == "" {
		methodTitle = "Fixed"
	}

	return []*Rate{{
		CarrierCode:  "flatrate",
		CarrierTitle: title,
		MethodCode:   "flatrate",
		MethodTitle:  methodTitle,
		Price:        price,
	}}, nil
}
