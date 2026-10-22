package btcpay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const (
	fakeStore = "Cco6QBtYubYNtf3TWo8j3TtNdwVio9tyBXSmFCDoJpk9" // the lab store's id: the captures name it
	fakeKey   = "fake-api-key"
)

// fakeBTCPay is a Greenfield server in a test: it records what the
// provider sent and answers from what the test put in it (invoices and
// payment methods — usually the lab's captures).
type fakeBTCPay struct {
	t  *testing.T
	ts *httptest.Server

	mu        sync.Mutex
	created   []CreateInvoiceRequest
	invoices  map[string]json.RawMessage
	methods   map[string]json.RawMessage
	marked    []string
	healthErr int // a status to answer /health with (0 = 200 synchronized)
	storeErr  int
	fetchErr  int // a status to answer every invoice GET with
	markErr   int // a status to answer every MarkStatus with (0 = the invoice)
	nextID    int
	calls     map[string]int
	unknown   []byte
}

func newFakeBTCPay(t *testing.T) *fakeBTCPay {
	unknown, err := os.ReadFile(filepath.Join("testdata", "get_invoice_unknown.403.json"))
	require.NoError(t, err)
	f := &fakeBTCPay{t: t, invoices: map[string]json.RawMessage{}, methods: map[string]json.RawMessage{}, calls: map[string]int{}, unknown: unknown}
	f.ts = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeBTCPay) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "token "+fakeKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.calls[r.Method+" "+r.URL.Path]++
	store := "/api/v1/stores/" + fakeStore
	switch {
	case r.URL.Path == "/api/v1/health":
		if f.healthErr != 0 {
			w.WriteHeader(f.healthErr)
			return
		}
		_, _ = io.WriteString(w, `{"synchronized":true}`)
	case r.URL.Path == store+"/invoices" && r.Method == http.MethodGet:
		// The readiness probe (?take=1).
		if f.storeErr != 0 {
			w.WriteHeader(f.storeErr)
			_, _ = io.WriteString(w, `{"code":"missing-permission","message":"x"}`)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	case r.URL.Path == store+"/invoices" && r.Method == http.MethodPost:
		var req CreateInvoiceRequest
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&req))
		f.created = append(f.created, req)
		f.nextID++
		id := fmt.Sprintf("FakeInv%d", f.nextID)
		meta, _ := json.Marshal(req.Metadata)
		inv := fmt.Sprintf(`{"id":%q,"storeId":%q,"amount":%q,"currency":%q,"status":"New","additionalStatus":"None","checkoutLink":"https://pay.test/i/%s","metadata":%s}`,
			id, fakeStore, req.Amount, req.Currency, id, meta)
		f.invoices[id] = json.RawMessage(inv)
		_, _ = io.WriteString(w, inv)
	case strings.HasPrefix(r.URL.Path, store+"/invoices/"):
		rest := strings.TrimPrefix(r.URL.Path, store+"/invoices/")
		parts := strings.Split(rest, "/")
		id := parts[0]
		if r.Method == http.MethodGet && f.fetchErr != 0 {
			w.WriteHeader(f.fetchErr)
			return
		}
		switch {
		case len(parts) == 1 && r.Method == http.MethodGet:
			inv, ok := f.invoices[id]
			if !ok {
				// BTCPay 2.4's answer for an invoice it does not know (the
				// lab's get_invoice_unknown.403.json).
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write(f.unknown)
				return
			}
			_, _ = w.Write(inv)
		case len(parts) == 2 && parts[1] == "payment-methods":
			pm, ok := f.methods[id]
			if !ok {
				pm = json.RawMessage(`[]`)
			}
			_, _ = w.Write(pm)
		case len(parts) == 2 && parts[1] == "status" && r.Method == http.MethodPost:
			if f.markErr != 0 {
				w.WriteHeader(f.markErr)
				_, _ = io.WriteString(w, `{"code":"invalid-state","message":"x"}`)
				return
			}
			f.marked = append(f.marked, id)
			_, _ = w.Write(f.invoices[id])
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// fixture: a tenant of its own on the test DB and a provider over the fake.
type fixture struct {
	db       *sql.DB
	tenantID string
	fake     *fakeBTCPay
	p        *Provider
	secret   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tenantID := fmt.Sprintf("btc-%d-%d", time.Now().UnixNano(), rand.Intn(1_000_000))
	_, err = db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'BTC', $2)`, tenantID, tenantID+"@test.local")
	require.NoError(t, err)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.CreateTenant(context.Background(), tenantID, "free", usage.FreeTierLimits.StorageBytes))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM payment_events WHERE tenant_id = $1 OR event_id LIKE $2`, tenantID, "fx-"+tenantID+"%")
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM payments WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	fake := newFakeBTCPay(t)
	secret := "fixture-secret-" + tenantID
	p := New(Config{URL: fake.ts.URL, StoreID: fakeStore, APIKey: fakeKey, WebhookSecret: secret}, db, zap.NewNop())
	return &fixture{db: db, tenantID: tenantID, fake: fake, p: p, secret: secret}
}

func (f *fixture) entitlement(t *testing.T) payments.Entitlement {
	t.Helper()
	e, err := payments.Get(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)
	return e
}

// ledger is the tenant's rows: provider_id → status.
func (f *fixture) ledger(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.db.Query(`SELECT provider_id, status FROM payments WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, st string
		require.NoError(t, rows.Scan(&id, &st))
		out[id] = st
	}
	require.NoError(t, rows.Err())
	return out
}

func (f *fixture) credit(t *testing.T) int64 {
	t.Helper()
	c, err := payments.CreditBalance(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)
	return c
}

// counterValue reads one series of the payments package's counters
// (provider, kind "" = no kind label, outcome).
func counterValue(name, provider, kind, outcome string) float64 {
	reg := prometheus.NewRegistry()
	reg.MustRegister(payments.Collectors()...)
	mfs, err := reg.Gather()
	if err != nil {
		return -1
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			if l["provider"] == provider && l["outcome"] == outcome && (kind == "" || l["kind"] == kind) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}
