# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project: magento2-cart-graphql-go

Go drop-in replacement for Magento 2's cart/checkout GraphQL. Write-heavy, stateful operations with tax calculation and order placement.

## Current State

**Phase 1: Complete (9/9 tasks).** Full guest checkout flow verified field-by-field against Magento PHP: cart creation, add-to-cart, addresses, shipping (flatrate per-item), payment (checkmo), tax, guest email, place order, and 8 tests (6 integration + 2 comparison).

## Build & Test

```bash
GOTOOLCHAIN=auto go build ./cmd/server/
GOTOOLCHAIN=auto go vet ./...
GOTOOLCHAIN=auto go run github.com/99designs/gqlgen generate

# Run server (port 8084)
MAGENTO_CRYPT_KEY="<key>" DB_USER=magento_go DB_PASSWORD=magento_go DB_NAME=magento248 GOTOOLCHAIN=auto go run ./cmd/server/
```

## Architecture

- **ConfigProvider** from day 1 — no raw `core_config_data` queries anywhere
- **Cart ID masking** — all external operations use 32-char masked IDs from `quote_id_mask`
- **Totals recalculation** — runs after every cart modification (add/remove/update/address/shipping)
- **Tax** — looks up `tax_calculation_rate` → `tax_calculation` → `tax_calculation_rule` matching country/region + product/customer tax class

## Key Database Tables

| Table | R/W | Purpose |
|-------|-----|---------|
| `quote` | R/W | Cart entity, totals |
| `quote_item` | R/W | Line items |
| `quote_address` | R/W | Billing + shipping addresses |
| `quote_payment` | R/W | Selected payment |
| `quote_id_mask` | R/W | Masked ID mapping |
| `quote_shipping_rate` | R/W | Available shipping rates |
| `shipping_tablerate` | R | Tablerate shipping lookup |
| `tax_calculation_rate` | R | Tax rates by country/region |
| `tax_calculation` | R | Rate → rule → product/customer class |
| `catalog_product_entity` | R | Product lookup for add-to-cart |
| `cataloginventory_stock_item` | R | Stock validation |

## Tax Scope

### What works
- US state-level tax (region_id based)
- Product tax class matching (only products with tax_class_id matching a rule get taxed)
- Customer tax class (default: Retail Customer, class 3)
- tax_calculation_rate → tax_calculation → tax_calculation_rule join

### What doesn't work yet
- EU VAT (country-level, no region) — #19
- Tax-inclusive pricing (price_includes_tax config) — #20
- Tax on shipping — #21
- Compound/stacked tax rules — #22
- FPT/WEEE tax

## Product Types

### Supported
- Simple products: add, update qty, remove, price, stock check

### Not yet
- Configurable: need selected_options decoding, parent+child quote_items — #11
- Bundle: need bundle option parsing, dynamic pricing — #12
- Virtual/Downloadable: need is_virtual cart detection, no shipping — #23
- Grouped: add children as individual simple items — #24

## Lessons Learned (from catalog + customer projects)

- DESCRIBE every table before writing SQL
- Use ConfigProvider for all core_config_data reads
- Never hardcode attribute IDs — use subqueries
- Always `redis-cli FLUSHALL` when testing after code changes
- Error messages must match Magento exactly (capitalized, with period)
- One PR per ticket, branch per feature
