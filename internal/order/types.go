// Package order handles the conversion from cart data to a sales_order and
// the transactional placement of that order in the database.
package order

// OrderInput holds all computed values needed to write a sales_order row and
// its related tables. It is produced by CartToOrder (pure conversion) and
// consumed by Place (SQL transaction). The IncrementID field is left empty
// by the converter and filled in by Place after reserving the sequence value.
type OrderInput struct {
	// Identity
	StoreID     int
	QuoteID     int
	IncrementID string // set by Place after sequence reservation

	// Customer
	CustomerID      *int
	CustomerIsGuest int
	CustomerGroupID int
	CustomerEmail   string
	Firstname       string // from billing address
	Lastname        string // from billing address

	// Cart flags
	IsVirtual  int
	CouponCode *string

	// Shipping
	ShippingMethod      string
	ShippingDescription string

	// Currency
	BaseCurrencyCode  string
	OrderCurrencyCode string

	// Totals
	Subtotal          float64
	SubtotalInclTax   float64
	ShippingAmount    float64
	ShippingTaxAmount float64
	ShippingInclTax   float64
	TaxAmount         float64
	DiscountAmount    float64
	GrandTotal        float64
	TotalQty          float64
	TotalItemCount    int

	// Items — in insertion order; children reference their parent by QuoteParentItemID
	Items []OrderItemInput

	// Addresses
	BillingAddr  *OrderAddressInput
	ShippingAddr *OrderAddressInput

	// Payment
	PaymentMethod string
}

// OrderItemInput holds per-item order data. QuoteParentItemID is the
// quote_item.parent_item_id of the source item; Place resolves this to the
// actual sales_order_item entity_id during the two-pass insert.
type OrderItemInput struct {
	QuoteItemID       int
	QuoteParentItemID *int // nil for top-level items

	ProductID       int
	ProductType     string
	SKU             string
	Name            string
	Qty             float64
	Price           float64
	RowTotal        float64
	PriceInclTax    float64
	RowTotalInclTax float64
	TaxPercent      float64
	TaxAmount       float64
	DiscountPercent float64
	DiscountAmount  float64
	IsVirtual      int
	StoreID        int
	ProductOptions string // JSON blob for sales_order_item.product_options; empty = NULL
}

// OrderAddressInput mirrors the fields written to sales_order_address.
type OrderAddressInput struct {
	AddressType string
	QuoteAddrID int
	CustomerID  *int
	Email       string
	Firstname   string
	Lastname    string
	Company     *string
	Street      string
	City        string
	Region      *string
	RegionID    *int
	Postcode    *string
	CountryID   string
	Telephone   *string
}
