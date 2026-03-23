# magento2-cart-graphql-go

High-performance Go drop-in replacement for Magento 2's cart and checkout GraphQL operations. Reads from and writes to the same MySQL database as Magento, producing identical cart totals and order placement behavior.

## Quick Start

```bash
git clone https://github.com/magendooro/magento2-cart-graphql-go.git
cd magento2-cart-graphql-go
GOTOOLCHAIN=auto go build -o server ./cmd/server/

MAGENTO_CRYPT_KEY="your-crypt-key" DB_USER=magento_go DB_PASSWORD=magento_go DB_NAME=magento248 ./server
```

Endpoints: GraphQL at `/graphql`, Playground at `/`, Health at `/health`.

Default port: **8084**.

## What's Implemented

### Queries
| Query | Status |
|-------|--------|
| `cart(cart_id)` | ✅ Fetch cart by masked ID |
| `customerCart` | ✅ Fetch authenticated customer's active cart |

### Mutations
| Mutation | Status | Notes |
|----------|--------|-------|
| `createEmptyCart` | ✅ | Returns 32-char masked ID |
| `createGuestCart` | ✅ | Returns full Cart object |
| `addProductsToCart` | ✅ | Simple products only (Phase 1) |
| `updateCartItems` | ✅ | Change quantity |
| `removeItemFromCart` | ✅ | Remove line item |
| `setShippingAddressesOnCart` | ✅ | With region resolution |
| `setBillingAddressOnCart` | ✅ | With same_as_shipping |
| `setShippingMethodsOnCart` | ✅ | Tablerate, flatrate, freeshipping |
| `setPaymentMethodOnCart` | ✅ | checkmo, free, banktransfer |
| `setGuestEmailOnCart` | ✅ | |
| `placeOrder` | 🔲 Phase 1h | Transactional quote→order |
| `applyCouponToCart` | 🔲 Phase 2a | Salesrule integration |
| `removeCouponFromCart` | 🔲 Phase 2a | |
| `mergeCarts` | 🔲 Phase 2d | Guest→customer merge |
| `estimateShippingMethods` | 🔲 Phase 2e | Non-committing estimate |

### Cart Features
| Feature | Status | Notes |
|---------|--------|-------|
| Cart ID masking | ✅ | Security: 32-char random IDs, never expose entity_id |
| Subtotal calculation | ✅ | Sum of item row_totals |
| US state tax | ✅ | Region-based rates from tax_calculation_rate |
| Product tax class | ✅ | Only tax_class_id > 0 products are taxed |
| Shipping rates | ✅ | Tablerate, flatrate, freeshipping carriers |
| Payment methods | ✅ | checkmo, free, banktransfer, cashondelivery |
| Duplicate SKU merge | ✅ | Same SKU increments quantity |
| Stock validation | ✅ | Checks cataloginventory_stock_item |
| EU VAT | 🔲 Phase 3a | Country-level, VAT ID, reverse charge |
| Tax-inclusive pricing | 🔲 Phase 3b | Catalog prices include tax |
| Tax on shipping | 🔲 Phase 3c | Shipping tax class |
| Configurable products | 🔲 Phase 2b | selected_options decoding |
| Bundle products | 🔲 Phase 2c | Bundle option selections |

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `DB_HOST` | `localhost` | MySQL host (localhost uses Unix socket) |
| `DB_USER` | `magento_go` | MySQL user |
| `DB_PASSWORD` | `magento_go` | MySQL password |
| `DB_NAME` | `magento248` | Magento database |
| `MAGENTO_CRYPT_KEY` | — | Required for JWT auth |
| `SERVER_PORT` | `8084` | HTTP listen port |
| `LOG_LEVEL` | `info` | debug, info, warn, error |

## Architecture

Same patterns as the catalog and customer Go microservices:
- Schema-first GraphQL via gqlgen
- ConfigProvider with Magento scope hierarchy (store → website → default)
- JWT authentication (Magento-compatible HS256)
- Repository pattern: one file per domain
- Unix socket MySQL connection

## License

MIT
