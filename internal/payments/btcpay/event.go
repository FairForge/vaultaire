package btcpay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxBodyBytes caps a webhook body (BTCPay payloads are small; 413 past it).
const MaxBodyBytes = 1 << 20

// Webhook event types (Greenfield WebhookEventType) the provider handles.
const (
	EventInvoiceCreated         = "InvoiceCreated"
	EventInvoiceReceivedPayment = "InvoiceReceivedPayment"
	EventInvoicePaymentSettled  = "InvoicePaymentSettled"
	EventInvoiceProcessing      = "InvoiceProcessing"
	EventInvoiceExpired         = "InvoiceExpired"
	EventInvoiceSettled         = "InvoiceSettled"
	EventInvoiceInvalid         = "InvoiceInvalid"
	// Seen in the lab, not in the design: right after InvoiceExpired
	// {partiallyPaid}, and after a late payment that covers the invoice.
	EventInvoiceExpiredPaidPartial  = "InvoiceExpiredPaidPartial"
	EventInvoicePaidAfterExpiration = "InvoicePaidAfterExpiration"
	// InvoiceRefund {pullPaymentId}: a refund opened (an operator's act,
	// recorded by hand — nothing to apply).
	EventInvoiceRefund = "InvoiceRefund"
)

// Event is one webhook delivery: the common fields and the invoice events'
// flags. The flags are informative only — every transition applies the
// FETCHED invoice (docs/PAYMENTS.md § 2.4).
type Event struct {
	DeliveryID         string `json:"deliveryId"`
	WebhookID          string `json:"webhookId"`
	OriginalDeliveryID string `json:"originalDeliveryId"`
	IsRedelivery       bool   `json:"isRedelivery"`
	Type               string `json:"type"`
	Timestamp          int64  `json:"timestamp"`
	StoreID            string `json:"storeId"`
	InvoiceID          string `json:"invoiceId"`

	Metadata        json.RawMessage `json:"metadata"`
	AfterExpiration bool            `json:"afterExpiration"`
	PartiallyPaid   bool            `json:"partiallyPaid"`
	OverPaid        bool            `json:"overPaid"`
	ManuallyMarked  bool            `json:"manuallyMarked"`
}

// DedupKey is the delivery's identity across redeliveries: a redelivery
// carries a new deliveryId and names the first in originalDeliveryId.
func (e Event) DedupKey() string {
	if e.OriginalDeliveryID != "" {
		return e.OriginalDeliveryID
	}
	return e.DeliveryID
}

// MetadataTenant is the tenant id the invoice's metadata names ("" none).
func (e Event) MetadataTenant() string { return metadataTenant(e.Metadata) }

func metadataTenant(raw json.RawMessage) string {
	var m struct {
		TenantID string `json:"tenant_id"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m.TenantID
}

// ErrBodyTooLarge: the body is over MaxBodyBytes (413).
var ErrBodyTooLarge = errors.New("btcpay webhook: body over the cap")

// ReadBody reads a delivery's body under the cap (http.MaxBytesReader, as
// the Stripe handler — never a truncating LimitReader).
func ReadBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, ErrBodyTooLarge
		}
		return nil, fmt.Errorf("btcpay webhook: read body: %w", err)
	}
	return body, nil
}

// ParseEvent decodes a verified body.
func ParseEvent(body []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return e, fmt.Errorf("btcpay webhook: decode: %w", err)
	}
	if e.DeliveryID == "" || e.Type == "" {
		return e, errors.New("btcpay webhook: no deliveryId or type")
	}
	return e, nil
}
