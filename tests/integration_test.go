package tests

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/99designs/gqlgen/graphql/handler"
	_ "github.com/go-sql-driver/mysql"

	"github.com/magendooro/magento2-cart-graphql-go/graph"
	appconfig "github.com/magendooro/magento2-cart-graphql-go/internal/config"
	"github.com/magendooro/magento2-cart-graphql-go/internal/jwt"
	"github.com/magendooro/magento2-cart-graphql-go/internal/middleware"
)

var testHandler http.Handler
var testDB *sql.DB

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func TestMain(m *testing.M) {
	host := envOrDefault("TEST_DB_HOST", "localhost")
	port := envOrDefault("TEST_DB_PORT", "3306")
	user := envOrDefault("TEST_DB_USER", "magento_go")
	password := envOrDefault("TEST_DB_PASSWORD", "magento_go")
	dbName := envOrDefault("TEST_DB_NAME", "magento248")
	socket := envOrDefault("TEST_DB_SOCKET", "/tmp/mysql.sock")

	var dsn string
	if host == "localhost" {
		dsn = user + ":" + password + "@unix(" + socket + ")/" + dbName + "?parseTime=true&time_zone=%27%2B00%3A00%27&loc=UTC"
	} else {
		dsn = user + ":" + password + "@tcp(" + host + ":" + port + ")/" + dbName + "?parseTime=true&time_zone=%27%2B00%3A00%27&loc=UTC"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		panic("failed to connect to test database: " + err.Error())
	}
	if err := db.Ping(); err != nil {
		panic("failed to ping test database: " + err.Error())
	}
	testDB = db

	cryptKey := envOrDefault("MAGENTO_CRYPT_KEY", "base64KjBr8ZM6bmK4xIWfk2/K0+xHEn+Ym6/Ogyl7Y7otzso=")
	jwtManager := jwt.NewManager(cryptKey, 60)

	cp, err := appconfig.NewConfigProvider(db)
	if err != nil {
		panic("failed to create config provider: " + err.Error())
	}

	resolver, err := graph.NewResolver(db, cp)
	if err != nil {
		panic("failed to create resolver: " + err.Error())
	}

	storeResolver := middleware.NewStoreResolver(db)
	tokenResolver := middleware.NewTokenResolver(db, jwtManager)
	resolver.TokenResolver = tokenResolver

	srv := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{
		Resolvers: resolver,
	}))

	mux := http.NewServeMux()
	mux.Handle("/graphql", srv)

	var h http.Handler = mux
	h = middleware.AuthMiddleware(tokenResolver)(h)
	h = middleware.StoreMiddleware(storeResolver)(h)
	testHandler = h

	os.Exit(m.Run())
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func doQuery(t *testing.T, query, token string) gqlResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"query": query})
	req := httptest.NewRequest("POST", "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Store", "default")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	testHandler.ServeHTTP(rr, req)

	var resp gqlResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v\nbody: %s", err, rr.Body.String())
	}
	return resp
}

// ─── Basic Cart Tests ──────────────────────────────────────────────────────

func TestCreateEmptyCart(t *testing.T) {
	resp := doQuery(t, `mutation { createEmptyCart }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected error: %s", resp.Errors[0].Message)
	}

	var data struct {
		CreateEmptyCart string `json:"createEmptyCart"`
	}
	json.Unmarshal(resp.Data, &data)
	if len(data.CreateEmptyCart) != 32 {
		t.Errorf("expected 32-char masked ID, got %q (len=%d)", data.CreateEmptyCart, len(data.CreateEmptyCart))
	}
}

func TestCreateGuestCart(t *testing.T) {
	resp := doQuery(t, `mutation { createGuestCart { cart { id total_quantity is_virtual } } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected error: %s", resp.Errors[0].Message)
	}

	var data struct {
		CreateGuestCart struct {
			Cart struct {
				ID            string  `json:"id"`
				TotalQuantity float64 `json:"total_quantity"`
				IsVirtual     bool    `json:"is_virtual"`
			} `json:"cart"`
		} `json:"createGuestCart"`
	}
	json.Unmarshal(resp.Data, &data)
	if len(data.CreateGuestCart.Cart.ID) != 32 {
		t.Errorf("expected 32-char masked ID, got %q", data.CreateGuestCart.Cart.ID)
	}
	if data.CreateGuestCart.Cart.TotalQuantity != 0 {
		t.Errorf("expected total_quantity 0, got %v", data.CreateGuestCart.Cart.TotalQuantity)
	}
}

func TestAddProductToCart(t *testing.T) {
	// Create cart
	resp := doQuery(t, `mutation { createEmptyCart }`, "")
	var createData struct {
		CreateEmptyCart string `json:"createEmptyCart"`
	}
	json.Unmarshal(resp.Data, &createData)
	cartID := createData.CreateEmptyCart

	// Add product
	resp = doQuery(t, `mutation {
		addProductsToCart(cartId: "`+cartID+`", cartItems: [{sku: "24-MB01", quantity: 1}]) {
			cart {
				total_quantity
				items { uid quantity product { sku name } prices { price { value currency } row_total { value } } }
			}
			user_errors { code message }
		}
	}`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected error: %s", resp.Errors[0].Message)
	}

	var data struct {
		AddProductsToCart struct {
			Cart struct {
				TotalQuantity float64 `json:"total_quantity"`
				Items         []struct {
					UID      string  `json:"uid"`
					Quantity float64 `json:"quantity"`
					Product  struct {
						SKU  string `json:"sku"`
						Name string `json:"name"`
					} `json:"product"`
					Prices struct {
						Price struct {
							Value    float64 `json:"value"`
							Currency string  `json:"currency"`
						} `json:"price"`
						RowTotal struct {
							Value float64 `json:"value"`
						} `json:"row_total"`
					} `json:"prices"`
				} `json:"items"`
			} `json:"cart"`
			UserErrors []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"user_errors"`
		} `json:"addProductsToCart"`
	}
	json.Unmarshal(resp.Data, &data)

	if data.AddProductsToCart.Cart.TotalQuantity != 1 {
		t.Errorf("expected total_quantity 1, got %v", data.AddProductsToCart.Cart.TotalQuantity)
	}
	if len(data.AddProductsToCart.Cart.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(data.AddProductsToCart.Cart.Items))
	}
	item := data.AddProductsToCart.Cart.Items[0]
	if item.Product.SKU != "24-MB01" {
		t.Errorf("expected SKU 24-MB01, got %s", item.Product.SKU)
	}
	if item.Prices.Price.Value != 34 {
		t.Errorf("expected price 34, got %v", item.Prices.Price.Value)
	}
	if len(data.AddProductsToCart.UserErrors) != 0 {
		t.Errorf("unexpected user errors: %v", data.AddProductsToCart.UserErrors)
	}
}

func TestAddInvalidProduct(t *testing.T) {
	resp := doQuery(t, `mutation { createEmptyCart }`, "")
	var createData struct {
		CreateEmptyCart string `json:"createEmptyCart"`
	}
	json.Unmarshal(resp.Data, &createData)
	cartID := createData.CreateEmptyCart

	resp = doQuery(t, `mutation {
		addProductsToCart(cartId: "`+cartID+`", cartItems: [{sku: "NONEXISTENT-SKU-999", quantity: 1}]) {
			cart { total_quantity }
			user_errors { code message }
		}
	}`, "")

	var data struct {
		AddProductsToCart struct {
			Cart struct {
				TotalQuantity float64 `json:"total_quantity"`
			} `json:"cart"`
			UserErrors []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"user_errors"`
		} `json:"addProductsToCart"`
	}
	json.Unmarshal(resp.Data, &data)

	if len(data.AddProductsToCart.UserErrors) == 0 {
		t.Fatal("expected user error for nonexistent product")
	}
	if data.AddProductsToCart.UserErrors[0].Code != "PRODUCT_NOT_FOUND" {
		t.Errorf("expected PRODUCT_NOT_FOUND, got %s", data.AddProductsToCart.UserErrors[0].Code)
	}
}

func TestInvalidCartID(t *testing.T) {
	resp := doQuery(t, `{ cart(cart_id: "nonexistent_cart_id_12345678") { id } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("expected error for invalid cart ID")
	}
}

func TestCartNotFound(t *testing.T) {
	resp := doQuery(t, `{ cart(cart_id: "aaaabbbbccccddddeeeeffffgggghhhh") { id } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("expected error for nonexistent cart")
	}
}
