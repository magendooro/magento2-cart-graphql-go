# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project: magento2-cart-graphql-go

Go drop-in replacement for Magento 2's cart/checkout GraphQL. Write-heavy, stateful operations with tax calculation and order placement. Verified field-by-field against Magento 2.4.8 PHP.

## Current State

**Phase 1 + Phase 2 + Phase 3: Complete.** 33 tests (28 integration + 5 comparison). EU VAT, tax on shipping, virtual products, structured PlaceOrder errors, discount propagation, compound/stacked tax, tax-inclusive pricing all done.

### What works (verified against Magento PHP)
- Cart creation (guest + customer), masked ID generation
- Add simple products (SKU lookup, status/stock validation, price from index)
- Update quantity, remove items, duplicate SKU merging
- Shipping addresses with region resolution (stores full name from directory_country_region)
- Billing addresses (explicit or same_as_shipping)
- Shipping methods: flatrate (per-item, default $5×qty), tablerate (website-scoped, price column)
- Payment methods: checkmo, banktransfer, cashondelivery, purchaseorder, free (from config)
- Guest email on cart
- Tax: US state-level + EU VAT, product tax class via eav_attribute.default_value fallback, compound/stacked rules, tax-inclusive pricing extraction
- Totals recalculation after every cart modification
- Place order: full transactional flow with correct sales_order fields, address ID backfill, grid data
- Coupon codes: applyCouponToCart/removeCouponFromCart with by_percent, by_fixed, cart_fixed via DiscountCollector pipeline

### Known gaps (documented as GitHub issues)
- `product_options` JSON not stored on sales_order_item
- `remote_ip` not captured on sales_order
- `email_sent` not set (Go doesn't send order emails)

## Build & Test

```bash
GOTOOLCHAIN=auto go build ./cmd/server/
GOTOOLCHAIN=auto go vet ./...
GOTOOLCHAIN=auto go run github.com/99designs/gqlgen generate

# Run all tests (requires MySQL + Magento at :8080 for comparison tests)
GOTOOLCHAIN=auto go test ./tests/ -v -timeout 300s -count=1

# Run only integration tests (no Magento needed)
GOTOOLCHAIN=auto go test ./tests/ -run "^Test[^C]" -v -timeout 60s -count=1

# Run server (port 8084)
MAGENTO_CRYPT_KEY="<key>" DB_USER=fch DB_NAME=magento248 GOTOOLCHAIN=auto go run ./cmd/server/
```

### Shared packages (from magento2-go-common)

`internal/{cache,config/provider,database,jwt,middleware}` were removed — provided by `magento2-go-common`. Cart uses aliased imports in `graph/resolver.go` and `internal/app/app.go`:

```go
"github.com/magendooro/magento2-go-common/config"      // ConfigProvider (passed as *config.ConfigProvider)
"github.com/magendooro/magento2-go-common/middleware"  // TokenResolver, AuthMiddleware, etc.
```

The service-local `internal/config/config.go` (Viper struct) is kept — it's the service's own config shape, not the shared ConfigProvider.

## Architecture

- **Totals Pipeline** (`internal/totals/`) — Magento-inspired collector pipeline: `SubtotalCollector(100) → ShippingCollector(350) → TaxCollector(450) → GrandTotalCollector(550)`. Adding coupons or tax-on-shipping = add one collector file. Single source of truth for totals (recalculate, display, and order all use same pipeline).
- **Carrier Registry** (`internal/shipping/`) — Strategy pattern: each carrier (flatrate, tablerate, freeshipping) implements `Carrier` interface. Registry auto-collects from active carriers. Adding new carriers = one file.
- **Error Constants** (`internal/errors/`) — All Magento-compatible error messages in one package. Grep-able, testable.
- **ConfigProvider** — no raw `core_config_data` queries anywhere
- **Cart ID masking** — all external operations use 32-char masked IDs from `quote_id_mask`
- **Tax** — batch `GetProductTaxClassIDs` loads all classes in single query. Falls back to `eav_attribute.default_value`.
- **Region resolution** — when `region_id` is provided, stores full name from `directory_country_region`
- **Shipping** — tablerate uses `price` column (not `cost`), scoped by `website_id`; flatrate defaults active with per-item pricing

## Key Database Tables

| Table | R/W | Purpose |
|-------|-----|---------|
| `quote` | R/W | Cart entity, totals |
| `quote_item` | R/W | Line items |
| `quote_address` | R/W | Billing + shipping addresses |
| `quote_payment` | R/W | Selected payment |
| `quote_id_mask` | R/W | Masked ID mapping |
| `shipping_tablerate` | R | Tablerate shipping lookup (price column, website-scoped) |
| `tax_calculation_rate` | R | Tax rates by country/region |
| `tax_calculation` | R | Rate → rule → product/customer class |
| `catalog_product_entity` | R | Product lookup for add-to-cart |
| `catalog_product_index_price` | R | Product pricing |
| `cataloginventory_stock_item` | R | Stock validation |
| `eav_attribute` | R | Attribute metadata + default values |
| `directory_country_region` | R | Region code/name resolution |
| `sales_order` | W | Order creation |
| `sales_order_item` | W | Order line items |
| `sales_order_address` | W | Order addresses |
| `sales_order_payment` | W | Order payment |
| `sales_order_grid` | W | Admin grid data |
| `inventory_reservation` | W | Stock reservation (negative qty) |
| `sequence_order_1` | W | Order increment ID reservation |

## Tax Scope

### What works
- US state-level tax (region_id based)
- EU VAT country-level tax (region_id=0) — verified with DE 19%
- Tax on shipping via ShippingTaxCollector (when tax/classes/shipping_tax_class configured)
- Product tax class matching via `eav_attribute.default_value` fallback
- Customer tax class (default: Retail Customer, class 3)
- tax_calculation_rate → tax_calculation → tax_calculation_rule join

### What doesn't work yet
- FPT/WEEE tax

## Product Types

### Supported
- Simple products: add, update qty, remove, price, stock check
- Configurable products: selected_options decoding, parent+child quote_items, ConfigurableCartItem response
- Bundle products: bundle option/selection decoding, dynamic pricing, BundleCartItem response
- Virtual/Downloadable: is_virtual auto-detected, shipping skipped for virtual carts

### Not yet
- Grouped: add children as individual simple items — #24

## Lessons Learned

### From this project
- DESCRIBE every table before writing SQL — `cost` vs `price` column burned us on tablerate
- `eav_attribute.default_value` matters — products without explicit EAV rows use it
- Magento stores full region names ("Texas"), not codes ("TX") — resolve via `directory_country_region`
- `sales_order` requires address ID backfill after inserting `sales_order_address`
- `store_to_base_rate` and `store_to_order_rate` are 0 in Magento (not 1)
- Grid address format: `street,city,region,postcode` — no name/country, comma without spaces
- Flatrate shipping is per-item by default (`type=I`, price × qty)
- PlaceOrder error messages must NOT be prefixed with "Unable to place order:"

### From this workspace (cross-cutting)
- Use ConfigProvider for all core_config_data reads — never query the table directly
- Never hardcode attribute IDs — use subqueries against eav_attribute
- Always `redis-cli FLUSHALL` when testing after code changes
- Error messages must match Magento exactly (capitalized, with period)
- One PR per ticket, branch per feature
- See parent workspace CLAUDE.md for go.work rules and common module guidelines
