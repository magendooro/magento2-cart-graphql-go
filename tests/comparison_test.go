package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// These comparison tests run the same queries against both the Go service (via httptest)
// and Magento PHP (at :8080), then compare the responses field by field.
//
// Run: GOTOOLCHAIN=auto go test ./tests/ -run TestCompare -v -timeout 300s -count=1
//
// Requirements:
//   - MySQL with Magento sample data (product 24-MB01 must exist)
//   - Magento PHP running at localhost:8080
//   - US tax rule configured (8.25% for Texas)

const magentoURL = "http://localhost:8080/graphql"

func doMagentoQuery(t *testing.T, query, token string) gqlResponse {
	t.Helper()
	body := `{"query":` + jsonString(query) + `}`
	req, err := http.NewRequest("POST", magentoURL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Store", "default")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Magento not available: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var gqlResp gqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		t.Fatalf("parse Magento response: %v\nbody: %s", err, string(respBody))
	}
	return gqlResp
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ─── Full Checkout Flow Comparison ──────────────────────────────────────────

// TestCompare_FullCheckoutFlow runs the complete checkout flow on both Go and
// Magento PHP, comparing the cart state after each mutation step.
func TestCompare_FullCheckoutFlow(t *testing.T) {
	// Step 1: Create cart on both
	goResp := doQuery(t, `mutation { createEmptyCart }`, "")
	mResp := doMagentoQuery(t, `mutation { createEmptyCart }`, "")

	var goCreate, mCreate struct {
		CreateEmptyCart string `json:"createEmptyCart"`
	}
	json.Unmarshal(goResp.Data, &goCreate)
	json.Unmarshal(mResp.Data, &mCreate)

	goCartID := goCreate.CreateEmptyCart
	mCartID := mCreate.CreateEmptyCart

	if len(goCartID) != 32 {
		t.Fatalf("Go cart ID not 32 chars: %q", goCartID)
	}
	if len(mCartID) != 32 {
		t.Fatalf("Magento cart ID not 32 chars: %q", mCartID)
	}
	t.Logf("Go cart: %s, Magento cart: %s", goCartID, mCartID)

	// Step 2: Add product
	addQuery := func(cartID string) string {
		return fmt.Sprintf(`mutation {
			addProductsToCart(cartId: "%s", cartItems: [{sku: "24-MB01", quantity: 2}]) {
				cart {
					total_quantity
					items { quantity product { sku } prices { price { value currency } row_total { value } } }
					prices { subtotal_excluding_tax { value } }
				}
				user_errors { code message }
			}
		}`, cartID)
	}

	goResp = doQuery(t, addQuery(goCartID), "")
	mResp = doMagentoQuery(t, addQuery(mCartID), "")
	compareAddProduct(t, goResp, mResp)

	// Step 3: Set shipping address
	setShippingQuery := func(cartID string) string {
		return fmt.Sprintf(`mutation {
			setShippingAddressesOnCart(input: {
				cart_id: "%s"
				shipping_addresses: [{
					address: {
						firstname: "Test"
						lastname: "User"
						street: ["123 Main St"]
						city: "Austin"
						region: "TX"
						region_id: 57
						postcode: "78701"
						country_code: "US"
						telephone: "5125551234"
					}
				}]
			}) {
				cart {
					shipping_addresses {
						firstname lastname city
						region { code label region_id }
						country { code }
						available_shipping_methods { carrier_code method_code amount { value } available }
					}
				}
			}
		}`, cartID)
	}

	goResp = doQuery(t, setShippingQuery(goCartID), "")
	mResp = doMagentoQuery(t, setShippingQuery(mCartID), "")
	compareShippingAddress(t, goResp, mResp)

	// Step 4: Set billing address
	setBillingQuery := func(cartID string) string {
		return fmt.Sprintf(`mutation {
			setBillingAddressOnCart(input: {
				cart_id: "%s"
				billing_address: {
					address: {
						firstname: "Test"
						lastname: "User"
						street: ["123 Main St"]
						city: "Austin"
						region: "TX"
						region_id: 57
						postcode: "78701"
						country_code: "US"
						telephone: "5125551234"
					}
				}
			}) {
				cart {
					billing_address { firstname lastname city country { code } }
				}
			}
		}`, cartID)
	}

	goResp = doQuery(t, setBillingQuery(goCartID), "")
	mResp = doMagentoQuery(t, setBillingQuery(mCartID), "")
	compareBillingAddress(t, goResp, mResp)

	// Step 5: Set shipping method
	setShipMethodQuery := func(cartID string) string {
		return fmt.Sprintf(`mutation {
			setShippingMethodsOnCart(input: {
				cart_id: "%s"
				shipping_methods: [{ carrier_code: "flatrate", method_code: "flatrate" }]
			}) {
				cart {
					shipping_addresses {
						selected_shipping_method { carrier_code method_code amount { value } }
					}
					prices { grand_total { value } subtotal_excluding_tax { value } }
				}
			}
		}`, cartID)
	}

	goResp = doQuery(t, setShipMethodQuery(goCartID), "")
	mResp = doMagentoQuery(t, setShipMethodQuery(mCartID), "")
	compareShippingMethod(t, goResp, mResp)

	// Step 6: Set payment method
	setPaymentQuery := func(cartID string) string {
		return fmt.Sprintf(`mutation {
			setPaymentMethodOnCart(input: {
				cart_id: "%s"
				payment_method: { code: "checkmo" }
			}) {
				cart {
					selected_payment_method { code }
					available_payment_methods { code title }
				}
			}
		}`, cartID)
	}

	goResp = doQuery(t, setPaymentQuery(goCartID), "")
	mResp = doMagentoQuery(t, setPaymentQuery(mCartID), "")
	comparePaymentMethod(t, goResp, mResp)

	// Step 7: Set guest email
	setEmailQuery := func(cartID string) string {
		return fmt.Sprintf(`mutation {
			setGuestEmailOnCart(input: {
				cart_id: "%s"
				email: "test-checkout@example.com"
			}) {
				cart { email }
			}
		}`, cartID)
	}

	goResp = doQuery(t, setEmailQuery(goCartID), "")
	mResp = doMagentoQuery(t, setEmailQuery(mCartID), "")
	compareEmail(t, goResp, mResp)

	// Step 8: Final cart state comparison before place order
	cartQuery := func(cartID string) string {
		return fmt.Sprintf(`{
			cart(cart_id: "%s") {
				email
				total_quantity
				is_virtual
				items { quantity product { sku } }
				prices {
					grand_total { value currency }
					subtotal_excluding_tax { value }
					subtotal_including_tax { value }
					applied_taxes { amount { value } label }
				}
				shipping_addresses {
					firstname lastname city
					selected_shipping_method { carrier_code method_code amount { value } }
				}
				billing_address { firstname lastname city }
				selected_payment_method { code }
			}
		}`, cartID)
	}

	goResp = doQuery(t, cartQuery(goCartID), "")
	mResp = doMagentoQuery(t, cartQuery(mCartID), "")
	compareFinalCart(t, goResp, mResp)

	// Step 9: Place order
	placeQuery := func(cartID string) string {
		return fmt.Sprintf(`mutation {
			placeOrder(input: { cart_id: "%s" }) {
				orderV2 { number }
			}
		}`, cartID)
	}

	goResp = doQuery(t, placeQuery(goCartID), "")
	mResp = doMagentoQuery(t, placeQuery(mCartID), "")
	comparePlaceOrder(t, goResp, mResp)
}

// ─── Comparison Helpers ─────────────────────────────────────────────────────

func compareAddProduct(t *testing.T, goResp, mResp gqlResponse) {
	t.Helper()
	if len(goResp.Errors) > 0 {
		t.Fatalf("Go addProducts error: %s", goResp.Errors[0].Message)
	}
	if len(mResp.Errors) > 0 {
		t.Fatalf("Magento addProducts error: %s", mResp.Errors[0].Message)
	}

	type addResp struct {
		AddProductsToCart struct {
			Cart struct {
				TotalQuantity float64 `json:"total_quantity"`
				Items         []struct {
					Quantity float64 `json:"quantity"`
					Product  struct {
						SKU string `json:"sku"`
					} `json:"product"`
					Prices struct {
						Price    struct{ Value float64 } `json:"price"`
						RowTotal struct{ Value float64 } `json:"row_total"`
					} `json:"prices"`
				} `json:"items"`
				Prices struct {
					SubtotalExcludingTax struct{ Value float64 } `json:"subtotal_excluding_tax"`
				} `json:"prices"`
			} `json:"cart"`
			UserErrors []struct{ Code, Message string } `json:"user_errors"`
		} `json:"addProductsToCart"`
	}

	var goData, mData addResp
	json.Unmarshal(goResp.Data, &goData)
	json.Unmarshal(mResp.Data, &mData)

	goCart := goData.AddProductsToCart.Cart
	mCart := mData.AddProductsToCart.Cart

	assertEq(t, "addProduct.total_quantity", goCart.TotalQuantity, mCart.TotalQuantity)
	assertEq(t, "addProduct.subtotal", goCart.Prices.SubtotalExcludingTax.Value, mCart.Prices.SubtotalExcludingTax.Value)

	if len(goCart.Items) != len(mCart.Items) {
		t.Errorf("item count: Go=%d Magento=%d", len(goCart.Items), len(mCart.Items))
		return
	}
	for i := range goCart.Items {
		assertEq(t, fmt.Sprintf("item[%d].sku", i), goCart.Items[i].Product.SKU, mCart.Items[i].Product.SKU)
		assertEq(t, fmt.Sprintf("item[%d].qty", i), goCart.Items[i].Quantity, mCart.Items[i].Quantity)
		assertEq(t, fmt.Sprintf("item[%d].price", i), goCart.Items[i].Prices.Price.Value, mCart.Items[i].Prices.Price.Value)
		assertEq(t, fmt.Sprintf("item[%d].row_total", i), goCart.Items[i].Prices.RowTotal.Value, mCart.Items[i].Prices.RowTotal.Value)
	}
	t.Log("PASS: addProductsToCart matches")
}

func compareShippingAddress(t *testing.T, goResp, mResp gqlResponse) {
	t.Helper()
	if len(goResp.Errors) > 0 {
		t.Fatalf("Go setShippingAddress error: %s", goResp.Errors[0].Message)
	}
	if len(mResp.Errors) > 0 {
		t.Fatalf("Magento setShippingAddress error: %s", mResp.Errors[0].Message)
	}

	type addrResp struct {
		SetShippingAddressesOnCart struct {
			Cart struct {
				ShippingAddresses []struct {
					Firstname string `json:"firstname"`
					Lastname  string `json:"lastname"`
					City      string `json:"city"`
					Region    struct {
						Code     string `json:"code"`
						Label    string `json:"label"`
						RegionID int    `json:"region_id"`
					} `json:"region"`
					Country struct {
						Code string `json:"code"`
					} `json:"country"`
					AvailableShippingMethods []struct {
						CarrierCode string `json:"carrier_code"`
						MethodCode  string `json:"method_code"`
						Available   bool   `json:"available"`
					} `json:"available_shipping_methods"`
				} `json:"shipping_addresses"`
			} `json:"cart"`
		} `json:"setShippingAddressesOnCart"`
	}

	var goData, mData addrResp
	json.Unmarshal(goResp.Data, &goData)
	json.Unmarshal(mResp.Data, &mData)

	goAddrs := goData.SetShippingAddressesOnCart.Cart.ShippingAddresses
	mAddrs := mData.SetShippingAddressesOnCart.Cart.ShippingAddresses

	if len(goAddrs) != len(mAddrs) {
		t.Errorf("shipping address count: Go=%d Magento=%d", len(goAddrs), len(mAddrs))
		return
	}
	if len(goAddrs) > 0 {
		assertEq(t, "shipping.firstname", goAddrs[0].Firstname, mAddrs[0].Firstname)
		assertEq(t, "shipping.city", goAddrs[0].City, mAddrs[0].City)
		assertEq(t, "shipping.region.code", goAddrs[0].Region.Code, mAddrs[0].Region.Code)
		assertEq(t, "shipping.country.code", goAddrs[0].Country.Code, mAddrs[0].Country.Code)

		// Compare available shipping methods (by carrier_code+method_code)
		goMethods := make(map[string]bool)
		for _, m := range goAddrs[0].AvailableShippingMethods {
			goMethods[m.CarrierCode+"_"+m.MethodCode] = true
		}
		for _, m := range mAddrs[0].AvailableShippingMethods {
			key := m.CarrierCode + "_" + m.MethodCode
			if !goMethods[key] {
				t.Errorf("Magento has shipping method %s but Go does not", key)
			}
		}
	}
	t.Log("PASS: setShippingAddressesOnCart matches")
}

func compareBillingAddress(t *testing.T, goResp, mResp gqlResponse) {
	t.Helper()
	if len(goResp.Errors) > 0 {
		t.Fatalf("Go setBillingAddress error: %s", goResp.Errors[0].Message)
	}
	if len(mResp.Errors) > 0 {
		t.Fatalf("Magento setBillingAddress error: %s", mResp.Errors[0].Message)
	}

	type billResp struct {
		SetBillingAddressOnCart struct {
			Cart struct {
				BillingAddress struct {
					Firstname string `json:"firstname"`
					Lastname  string `json:"lastname"`
					City      string `json:"city"`
					Country   struct{ Code string } `json:"country"`
				} `json:"billing_address"`
			} `json:"cart"`
		} `json:"setBillingAddressOnCart"`
	}

	var goData, mData billResp
	json.Unmarshal(goResp.Data, &goData)
	json.Unmarshal(mResp.Data, &mData)

	goAddr := goData.SetBillingAddressOnCart.Cart.BillingAddress
	mAddr := mData.SetBillingAddressOnCart.Cart.BillingAddress

	assertEq(t, "billing.firstname", goAddr.Firstname, mAddr.Firstname)
	assertEq(t, "billing.city", goAddr.City, mAddr.City)
	assertEq(t, "billing.country.code", goAddr.Country.Code, mAddr.Country.Code)
	t.Log("PASS: setBillingAddressOnCart matches")
}

func compareShippingMethod(t *testing.T, goResp, mResp gqlResponse) {
	t.Helper()
	if len(goResp.Errors) > 0 {
		t.Fatalf("Go setShippingMethods error: %s", goResp.Errors[0].Message)
	}
	if len(mResp.Errors) > 0 {
		t.Fatalf("Magento setShippingMethods error: %s", mResp.Errors[0].Message)
	}

	type shipResp struct {
		SetShippingMethodsOnCart struct {
			Cart struct {
				ShippingAddresses []struct {
					SelectedShippingMethod struct {
						CarrierCode string             `json:"carrier_code"`
						MethodCode  string             `json:"method_code"`
						Amount      struct{ Value float64 } `json:"amount"`
					} `json:"selected_shipping_method"`
				} `json:"shipping_addresses"`
				Prices struct {
					GrandTotal           struct{ Value float64 } `json:"grand_total"`
					SubtotalExcludingTax struct{ Value float64 } `json:"subtotal_excluding_tax"`
				} `json:"prices"`
			} `json:"cart"`
		} `json:"setShippingMethodsOnCart"`
	}

	var goData, mData shipResp
	json.Unmarshal(goResp.Data, &goData)
	json.Unmarshal(mResp.Data, &mData)

	goCart := goData.SetShippingMethodsOnCart.Cart
	mCart := mData.SetShippingMethodsOnCart.Cart

	if len(goCart.ShippingAddresses) > 0 && len(mCart.ShippingAddresses) > 0 {
		goShip := goCart.ShippingAddresses[0].SelectedShippingMethod
		mShip := mCart.ShippingAddresses[0].SelectedShippingMethod
		assertEq(t, "shipping_method.carrier_code", goShip.CarrierCode, mShip.CarrierCode)
		assertEq(t, "shipping_method.method_code", goShip.MethodCode, mShip.MethodCode)
		assertEq(t, "shipping_method.amount", goShip.Amount.Value, mShip.Amount.Value)
	}
	assertEq(t, "subtotal_excluding_tax", goCart.Prices.SubtotalExcludingTax.Value, mCart.Prices.SubtotalExcludingTax.Value)
	t.Logf("Go grand_total=%.2f, Magento grand_total=%.2f", goCart.Prices.GrandTotal.Value, mCart.Prices.GrandTotal.Value)
	assertEq(t, "grand_total", goCart.Prices.GrandTotal.Value, mCart.Prices.GrandTotal.Value)
	t.Log("PASS: setShippingMethodsOnCart matches")
}

func comparePaymentMethod(t *testing.T, goResp, mResp gqlResponse) {
	t.Helper()
	if len(goResp.Errors) > 0 {
		t.Fatalf("Go setPaymentMethod error: %s", goResp.Errors[0].Message)
	}
	if len(mResp.Errors) > 0 {
		t.Fatalf("Magento setPaymentMethod error: %s", mResp.Errors[0].Message)
	}

	type payResp struct {
		SetPaymentMethodOnCart struct {
			Cart struct {
				SelectedPaymentMethod struct {
					Code string `json:"code"`
				} `json:"selected_payment_method"`
			} `json:"cart"`
		} `json:"setPaymentMethodOnCart"`
	}

	var goData, mData payResp
	json.Unmarshal(goResp.Data, &goData)
	json.Unmarshal(mResp.Data, &mData)

	assertEq(t, "payment.code",
		goData.SetPaymentMethodOnCart.Cart.SelectedPaymentMethod.Code,
		mData.SetPaymentMethodOnCart.Cart.SelectedPaymentMethod.Code)
	t.Log("PASS: setPaymentMethodOnCart matches")
}

func compareEmail(t *testing.T, goResp, mResp gqlResponse) {
	t.Helper()
	if len(goResp.Errors) > 0 {
		t.Fatalf("Go setGuestEmail error: %s", goResp.Errors[0].Message)
	}
	if len(mResp.Errors) > 0 {
		t.Fatalf("Magento setGuestEmail error: %s", mResp.Errors[0].Message)
	}

	type emailResp struct {
		SetGuestEmailOnCart struct {
			Cart struct {
				Email string `json:"email"`
			} `json:"cart"`
		} `json:"setGuestEmailOnCart"`
	}

	var goData, mData emailResp
	json.Unmarshal(goResp.Data, &goData)
	json.Unmarshal(mResp.Data, &mData)

	assertEq(t, "email",
		goData.SetGuestEmailOnCart.Cart.Email,
		mData.SetGuestEmailOnCart.Cart.Email)
	t.Log("PASS: setGuestEmailOnCart matches")
}

func compareFinalCart(t *testing.T, goResp, mResp gqlResponse) {
	t.Helper()
	if len(goResp.Errors) > 0 {
		t.Fatalf("Go cart query error: %s", goResp.Errors[0].Message)
	}
	if len(mResp.Errors) > 0 {
		t.Fatalf("Magento cart query error: %s", mResp.Errors[0].Message)
	}

	type cartResp struct {
		Cart struct {
			Email         string  `json:"email"`
			TotalQuantity float64 `json:"total_quantity"`
			IsVirtual     bool    `json:"is_virtual"`
			Items         []struct {
				Quantity float64 `json:"quantity"`
				Product  struct {
					SKU string `json:"sku"`
				} `json:"product"`
			} `json:"items"`
			Prices struct {
				GrandTotal           struct{ Value float64; Currency string } `json:"grand_total"`
				SubtotalExcludingTax struct{ Value float64 }                 `json:"subtotal_excluding_tax"`
				SubtotalIncludingTax struct{ Value float64 }                 `json:"subtotal_including_tax"`
				AppliedTaxes         []struct {
					Amount struct{ Value float64 } `json:"amount"`
					Label  string                  `json:"label"`
				} `json:"applied_taxes"`
			} `json:"prices"`
			ShippingAddresses []struct {
				Firstname              string `json:"firstname"`
				SelectedShippingMethod struct {
					CarrierCode string             `json:"carrier_code"`
					MethodCode  string             `json:"method_code"`
					Amount      struct{ Value float64 } `json:"amount"`
				} `json:"selected_shipping_method"`
			} `json:"shipping_addresses"`
			BillingAddress struct {
				Firstname string `json:"firstname"`
			} `json:"billing_address"`
			SelectedPaymentMethod struct {
				Code string `json:"code"`
			} `json:"selected_payment_method"`
		} `json:"cart"`
	}

	var goData, mData cartResp
	json.Unmarshal(goResp.Data, &goData)
	json.Unmarshal(mResp.Data, &mData)

	goCart := goData.Cart
	mCart := mData.Cart

	assertEq(t, "final.email", goCart.Email, mCart.Email)
	assertEq(t, "final.total_quantity", goCart.TotalQuantity, mCart.TotalQuantity)
	assertEq(t, "final.is_virtual", goCart.IsVirtual, mCart.IsVirtual)
	assertEq(t, "final.subtotal_excluding_tax", goCart.Prices.SubtotalExcludingTax.Value, mCart.Prices.SubtotalExcludingTax.Value)
	assertEq(t, "final.subtotal_including_tax", goCart.Prices.SubtotalIncludingTax.Value, mCart.Prices.SubtotalIncludingTax.Value)
	assertEq(t, "final.grand_total", goCart.Prices.GrandTotal.Value, mCart.Prices.GrandTotal.Value)
	assertEq(t, "final.payment.code", goCart.SelectedPaymentMethod.Code, mCart.SelectedPaymentMethod.Code)

	// Compare applied taxes
	t.Logf("Go applied_taxes: %d, Magento applied_taxes: %d", len(goCart.Prices.AppliedTaxes), len(mCart.Prices.AppliedTaxes))
	if len(goCart.Prices.AppliedTaxes) == len(mCart.Prices.AppliedTaxes) {
		for i := range goCart.Prices.AppliedTaxes {
			assertEq(t, fmt.Sprintf("final.tax[%d].amount", i), goCart.Prices.AppliedTaxes[i].Amount.Value, mCart.Prices.AppliedTaxes[i].Amount.Value)
			assertEq(t, fmt.Sprintf("final.tax[%d].label", i), goCart.Prices.AppliedTaxes[i].Label, mCart.Prices.AppliedTaxes[i].Label)
		}
	} else {
		t.Errorf("applied_taxes count: Go=%d Magento=%d", len(goCart.Prices.AppliedTaxes), len(mCart.Prices.AppliedTaxes))
	}

	t.Log("PASS: final cart state matches")
}

func comparePlaceOrder(t *testing.T, goResp, mResp gqlResponse) {
	t.Helper()
	if len(goResp.Errors) > 0 {
		t.Fatalf("Go placeOrder error: %s", goResp.Errors[0].Message)
	}
	if len(mResp.Errors) > 0 {
		t.Fatalf("Magento placeOrder error: %s", mResp.Errors[0].Message)
	}

	type placeResp struct {
		PlaceOrder struct {
			OrderV2 struct {
				Number string `json:"number"`
			} `json:"orderV2"`
		} `json:"placeOrder"`
	}

	var goData, mData placeResp
	json.Unmarshal(goResp.Data, &goData)
	json.Unmarshal(mResp.Data, &mData)

	goNum := goData.PlaceOrder.OrderV2.Number
	mNum := mData.PlaceOrder.OrderV2.Number

	if goNum == "" {
		t.Error("Go order number is empty")
	}
	if mNum == "" {
		t.Error("Magento order number is empty")
	}

	// Order numbers will differ — just verify both returned valid increment IDs (9-digit zero-padded)
	if len(goNum) != 9 {
		t.Errorf("Go order number unexpected length: %q (expected 9 digits)", goNum)
	}
	if len(mNum) != 9 {
		t.Errorf("Magento order number unexpected length: %q (expected 9 digits)", mNum)
	}

	t.Logf("PASS: placeOrder succeeded — Go=#%s, Magento=#%s", goNum, mNum)
}

// ─── Error Behavior Comparison ──────────────────────────────────────────────

func TestCompare_EmptyCartPlaceOrder(t *testing.T) {
	// Both should error when trying to place an order on an empty cart
	goResp := doQuery(t, `mutation { createEmptyCart }`, "")
	mResp := doMagentoQuery(t, `mutation { createEmptyCart }`, "")

	var goCreate, mCreate struct {
		CreateEmptyCart string `json:"createEmptyCart"`
	}
	json.Unmarshal(goResp.Data, &goCreate)
	json.Unmarshal(mResp.Data, &mCreate)

	placeQuery := func(cartID string) string {
		return fmt.Sprintf(`mutation { placeOrder(input: { cart_id: "%s" }) { orderV2 { number } } }`, cartID)
	}

	goResp = doQuery(t, placeQuery(goCreate.CreateEmptyCart), "")
	mResp = doMagentoQuery(t, placeQuery(mCreate.CreateEmptyCart), "")

	if len(goResp.Errors) == 0 {
		t.Error("Go should error on empty cart placeOrder")
	} else {
		t.Logf("Go error: %s", goResp.Errors[0].Message)
	}
	if len(mResp.Errors) == 0 {
		t.Logf("Magento did not error on empty cart placeOrder (may return null data)")
	} else {
		t.Logf("Magento error: %s", mResp.Errors[0].Message)
	}
	// Both should produce errors for empty cart
	if len(goResp.Errors) > 0 && len(mResp.Errors) > 0 {
		t.Log("PASS: both error on empty cart placeOrder")
	} else if len(goResp.Errors) > 0 {
		t.Log("PASS: Go correctly rejects empty cart (Magento behavior may differ)")
	}
}

// ─── Assertion Helpers ──────────────────────────────────────────────────────

func assertEq(t *testing.T, field string, goVal, magentoVal any) {
	t.Helper()
	goStr := fmt.Sprintf("%v", goVal)
	mStr := fmt.Sprintf("%v", magentoVal)
	if goStr != mStr {
		t.Errorf("%s mismatch:\n  Go:      %v\n  Magento: %v", field, goVal, magentoVal)
	}
}
