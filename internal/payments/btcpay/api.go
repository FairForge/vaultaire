package btcpay

import (
	"context"
	"encoding/json"
	"net/http"
)

// The Greenfield calls the provider makes, in the shapes BTCPay answered in
// the lab (docs/PAYMENTS.md § 3.2). Amounts are decimal strings, as the API
// sends them; the provider converts at the edges.

// Invoice statuses and additional statuses (Greenfield InvoiceStatus /
// InvoiceAdditionalStatus).
const (
	StatusNew        = "New"
	StatusProcessing = "Processing"
	StatusExpired    = "Expired"
	StatusInvalid    = "Invalid"
	StatusSettled    = "Settled"

	AddNone        = "None"
	AddPaidLate    = "PaidLate"
	AddPaidPartial = "PaidPartial"
	AddMarked      = "Marked"
	AddInvalid     = "Invalid"
	AddPaidOver    = "PaidOver"
)

// Speed policies (checkout.speedPolicy): how many confirmations make an
// invoice Settled.
const (
	SpeedHigh      = "HighSpeed"
	SpeedMedium    = "MediumSpeed"
	SpeedLowMedium = "LowMediumSpeed"
	SpeedLow       = "LowSpeed"
)

// CheckoutOptions is the invoice's checkout block.
type CheckoutOptions struct {
	SpeedPolicy           string   `json:"speedPolicy,omitempty"`
	PaymentMethods        []string `json:"paymentMethods,omitempty"`
	ExpirationMinutes     int      `json:"expirationMinutes,omitempty"`
	MonitoringMinutes     int      `json:"monitoringMinutes,omitempty"`
	PaymentTolerance      *float64 `json:"paymentTolerance,omitempty"`
	RedirectURL           string   `json:"redirectURL,omitempty"`
	RedirectAutomatically bool     `json:"redirectAutomatically,omitempty"`
}

// ReceiptOptions is the invoice's receipt block.
type ReceiptOptions struct {
	Enabled      *bool `json:"enabled,omitempty"`
	ShowPayments *bool `json:"showPayments,omitempty"`
}

// CreateInvoiceRequest is POST /api/v1/stores/{store}/invoices.
type CreateInvoiceRequest struct {
	Amount   string          `json:"amount"`
	Currency string          `json:"currency"`
	Metadata map[string]any  `json:"metadata,omitempty"`
	Checkout CheckoutOptions `json:"checkout"`
	Receipt  *ReceiptOptions `json:"receipt,omitempty"`
}

// Invoice is Greenfield's InvoiceData (the fields the provider reads).
type Invoice struct {
	ID               string          `json:"id"`
	StoreID          string          `json:"storeId"`
	Amount           string          `json:"amount"`
	PaidAmount       string          `json:"paidAmount"`
	Currency         string          `json:"currency"`
	Status           string          `json:"status"`
	AdditionalStatus string          `json:"additionalStatus"`
	CheckoutLink     string          `json:"checkoutLink"`
	CreatedTime      int64           `json:"createdTime"`
	ExpirationTime   int64           `json:"expirationTime"`
	MonitoringExpiry int64           `json:"monitoringExpiration"`
	Metadata         json.RawMessage `json:"metadata"`
}

// Payment is one on-chain payment of an invoice's payment method.
type Payment struct {
	ID           string `json:"id"`
	ReceivedDate int64  `json:"receivedDate"`
	Value        string `json:"value"`
	Fee          string `json:"fee"`
	Status       string `json:"status"` // Invalid | Processing | Settled
	Destination  string `json:"destination"`
}

// PaymentMethod is one entry of GET …/invoices/{id}/payment-methods.
type PaymentMethod struct {
	PaymentMethodID   string    `json:"paymentMethodId"` // BTCPay 2.x ("BTC-CHAIN")
	PaymentMethod     string    `json:"paymentMethod"`   // BTCPay 1.x ("BTC")
	Currency          string    `json:"currency"`
	Destination       string    `json:"destination"`
	Rate              string    `json:"rate"`
	PaymentMethodPaid string    `json:"paymentMethodPaid"`
	TotalPaid         string    `json:"totalPaid"`
	Due               string    `json:"due"`
	Amount            string    `json:"amount"`
	PaymentMethodFee  string    `json:"paymentMethodFee"`
	Payments          []Payment `json:"payments"`
}

// CreateInvoice creates an invoice in the store.
func (c *Client) CreateInvoice(ctx context.Context, req CreateInvoiceRequest) (Invoice, error) {
	var inv Invoice
	err := c.do(ctx, http.MethodPost, c.storePath("invoices"), req, &inv)
	return inv, err
}

// GetInvoice fetches an invoice (IsNotFound for one the store lacks).
func (c *Client) GetInvoice(ctx context.Context, invoiceID string) (Invoice, error) {
	var inv Invoice
	err := c.do(ctx, http.MethodGet, c.storePath("invoices", invoiceID), nil, &inv)
	return inv, err
}

// GetPaymentMethods fetches an invoice's payment methods (rate, paid, due,
// the payments).
func (c *Client) GetPaymentMethods(ctx context.Context, invoiceID string) ([]PaymentMethod, error) {
	var pms []PaymentMethod
	err := c.do(ctx, http.MethodGet, c.storePath("invoices", invoiceID, "payment-methods"), nil, &pms)
	return pms, err
}

// MarkStatus marks an invoice Settled or Invalid (the paid-late
// automation; needs canmodifyinvoices).
func (c *Client) MarkStatus(ctx context.Context, invoiceID, status string) (Invoice, error) {
	var inv Invoice
	err := c.do(ctx, http.MethodPost, c.storePath("invoices", invoiceID, "status"), map[string]string{"status": status}, &inv)
	return inv, err
}

// RefundRequest is POST …/invoices/{id}/refund (a pull payment the
// customer claims; the owner approves and signs the payout — manual by
// design).
type RefundRequest struct {
	Name               string  `json:"name,omitempty"`
	Description        string  `json:"description,omitempty"`
	PayoutMethodID     string  `json:"payoutMethodId,omitempty"` // 2.x
	PaymentMethod      string  `json:"paymentMethod,omitempty"`  // 1.x
	RefundVariant      string  `json:"refundVariant"`            // RateThen | CurrentRate | OverpaidAmount | Fiat | Custom
	SubtractPercentage float64 `json:"subtractPercentage,omitempty"`
	CustomAmount       string  `json:"customAmount,omitempty"`
	CustomCurrency     string  `json:"customCurrency,omitempty"`
}

// PullPayment is the refund's answer.
type PullPayment struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
	ViewLink string `json:"viewLink"`
}

// CreateRefund opens a refund (pull payment) for an invoice.
func (c *Client) CreateRefund(ctx context.Context, invoiceID string, req RefundRequest) (PullPayment, error) {
	var pp PullPayment
	err := c.do(ctx, http.MethodPost, c.storePath("invoices", invoiceID, "refund"), req, &pp)
	return pp, err
}

// Health is GET /api/v1/health.
func (c *Client) Health(ctx context.Context) (bool, error) {
	var h struct {
		Synchronized bool `json:"synchronized"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/health", nil, &h); err != nil {
		return false, err
	}
	return h.Synchronized, nil
}

// ProbeInvoices is GET /api/v1/stores/{store}/invoices?take=1 — the
// store read the scoped key CAN make (canviewinvoices; GET the store
// itself needs canviewstoresettings and answers 403 — the lab).
func (c *Client) ProbeInvoices(ctx context.Context) error {
	var out []json.RawMessage
	return c.do(ctx, http.MethodGet, c.storePath("invoices")+"?take=1", nil, &out)
}
