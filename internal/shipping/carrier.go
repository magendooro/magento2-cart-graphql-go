// Package shipping implements the carrier abstraction for shipping rate
// collection. Each carrier implements the Carrier interface and is
// registered with the Registry. Inspired by Magento's AbstractCarrierInterface.
package shipping

import (
	"context"

	"github.com/rs/zerolog/log"
)

// RateRequest holds the data carriers need to compute rates.
type RateRequest struct {
	StoreID   int
	WebsiteID int
	CountryID string
	RegionID  *int
	Postcode  *string
	Subtotal  float64
	ItemQty   float64
}

// Rate holds a single available shipping rate.
type Rate struct {
	CarrierCode  string
	CarrierTitle string
	MethodCode   string
	MethodTitle  string
	Price        float64
}

// Carrier computes shipping rates for a given request.
type Carrier interface {
	Code() string
	IsActive(storeID int) bool
	CollectRates(ctx context.Context, req *RateRequest) ([]*Rate, error)
}

// Registry holds all known carriers and collects rates from active ones.
type Registry struct {
	carriers []Carrier
}

// NewRegistry creates a carrier registry with the given carriers.
func NewRegistry(carriers ...Carrier) *Registry {
	return &Registry{carriers: carriers}
}

// CollectRates returns rates from all active carriers.
// A failing carrier is logged but does not block other carriers.
func (r *Registry) CollectRates(ctx context.Context, req *RateRequest) []*Rate {
	var rates []*Rate
	for _, c := range r.carriers {
		if !c.IsActive(req.StoreID) {
			continue
		}
		got, err := c.CollectRates(ctx, req)
		if err != nil {
			log.Debug().Str("carrier", c.Code()).Err(err).Msg("carrier rate collection failed")
			continue
		}
		rates = append(rates, got...)
	}
	return rates
}
