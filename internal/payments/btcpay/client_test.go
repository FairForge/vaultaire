package btcpay

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Greenfield client: the store-scoped key as `Authorization: token`,
// one retry on a fresh connection for a GET that got no answer, never for
// a POST; BTCPay 2.4's 403 missing-permission is "no such invoice", a 401
// is not.

func TestClient_TokenHeader_UnknownInvoiceIs403(t *testing.T) {
	unknown, err := os.ReadFile(filepath.Join("testdata", "get_invoice_unknown.403.json"))
	require.NoError(t, err)
	badKey, err := os.ReadFile(filepath.Join("testdata", "bad_key.401.json"))
	require.NoError(t, err)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "token good":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(unknown)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(badKey)
		}
	}))
	t.Cleanup(ts.Close)

	_, err = NewClient(ts.URL, "S", "good").GetInvoice(context.Background(), "nope")
	require.Error(t, err)
	assert.True(t, IsUnknownInvoice(err))
	assert.NotContains(t, err.Error(), "good", "the key is never in an error")

	_, err = NewClient(ts.URL, "S", "wrong").GetInvoice(context.Background(), "nope")
	require.Error(t, err)
	assert.False(t, IsUnknownInvoice(err), "a wrong key is not an unknown invoice")
}

// A GET whose connection dies before any answer is sent once more on a
// fresh connection; a POST is not (it could create a second invoice).
func TestClient_RetriesGETOnceOnAFreshConnection_NeverPOST(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// Hang up without an answer.
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"synchronized":true}`))
	}))
	t.Cleanup(ts.Close)
	c := NewClient(ts.URL, "S", "k")

	ok, err := c.Health(context.Background())
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int32(2), hits.Load())

	hits.Store(0)
	_, err = c.CreateInvoice(context.Background(), CreateInvoiceRequest{Amount: "1.00", Currency: "USD"})
	require.Error(t, err)
	assert.Equal(t, int32(1), hits.Load(), "a POST is never retried")
}

// The health body decides: BTCPay answers 200 {"synchronized":false}
// while its node is down (the lab's health_nbxplorer_down.200.json).
func TestClient_HealthReadsTheBody(t *testing.T) {
	down, err := os.ReadFile(filepath.Join("testdata", "health_nbxplorer_down.200.json"))
	require.NoError(t, err)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(down) }))
	t.Cleanup(ts.Close)

	ok, err := NewClient(ts.URL, "S", "k").Health(context.Background())
	require.NoError(t, err)
	assert.False(t, ok)
}

// The client's shapes decode what BTCPay answered in the lab: every
// capture's invoice and payment methods, and its delivery body.
func TestCaptures_DecodeIntoTheClientShapes(t *testing.T) {
	for _, n := range captureNames(t) {
		ev, err := ParseEvent(loadCapture(t, n).Raw)
		require.NoError(t, err, n)
		assert.Equal(t, ev.DeliveryID != ev.OriginalDeliveryID, ev.IsRedelivery, "%s: originalDeliveryId names the first delivery", n)
		if _, err := os.Stat(filepath.Join("testdata", n+".invoice.json")); err != nil {
			continue
		}
		var inv Invoice
		require.NoError(t, json.Unmarshal(companion(t, n, "invoice"), &inv), n)
		assert.Equal(t, ev.InvoiceID, inv.ID, n)
		assert.NotEmpty(t, inv.Status, n)
		var pms []PaymentMethod
		require.NoError(t, json.Unmarshal(companion(t, n, "payment-methods"), &pms), n)
		require.Len(t, pms, 1, n)
		assert.Equal(t, "BTC-CHAIN", pms[0].PaymentMethodID, n)
	}
}
