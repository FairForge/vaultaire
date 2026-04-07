package billing

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/stripe/stripe-go/v75"
	portalsession "github.com/stripe/stripe-go/v75/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v75/checkout/session"
	"github.com/stripe/stripe-go/v75/customer"
	"github.com/stripe/stripe-go/v75/invoice"
	"github.com/stripe/stripe-go/v75/subscription"
	"go.uber.org/zap"
)

// Plan holds a Stripe Price ID and display metadata for a product tier.
type Plan struct {
	ID        string // internal name (e.g. "vault3")
	PriceID   string // Stripe Price ID (e.g. "price_xxx")
	Name      string // display name
	PriceFmt  string // e.g. "$2.99/mo"
	StorageTB int64  // storage limit in TB
}

// InvoiceRow is a single invoice for dashboard display.
type InvoiceRow struct {
	ID          string
	Date        string
	Amount      string
	Status      string
	StatusClass string
	PDFURL      string
}

// StripeService manages Stripe customer, subscription, and billing operations.
type StripeService struct {
	db     *sql.DB
	logger *zap.Logger
	plans  map[string]Plan
}

// NewStripeService initializes the Stripe SDK and returns a new service.
// apiKey is the Stripe secret key (sk_test_... or sk_live_...).
func NewStripeService(apiKey string, db *sql.DB, logger *zap.Logger) *StripeService {
	stripe.Key = apiKey
	return &StripeService{
		db:     db,
		logger: logger,
		plans:  make(map[string]Plan),
	}
}

// RegisterPlan adds a plan that can be used in checkout sessions.
// Call this at startup with Price IDs from your Stripe Dashboard.
func (s *StripeService) RegisterPlan(p Plan) {
	s.plans[p.ID] = p
}

// GetPlan returns a registered plan by ID.
func (s *StripeService) GetPlan(id string) (Plan, bool) {
	p, ok := s.plans[id]
	return p, ok
}

// Plans returns all registered plans.
func (s *StripeService) Plans() []Plan {
	out := make([]Plan, 0, len(s.plans))
	for _, p := range s.plans {
		out = append(out, p)
	}
	return out
}

// --- Customer ---

// CreateCustomer creates a Stripe customer and optionally persists the
// stripe_customer_id to the tenants table.
func (s *StripeService) CreateCustomer(ctx context.Context, email, tenantID string) (string, error) {
	params := &stripe.CustomerParams{
		Email: stripe.String(email),
		Metadata: map[string]string{
			"tenant_id": tenantID,
		},
	}

	c, err := customer.New(params)
	if err != nil {
		return "", fmt.Errorf("create stripe customer: %w", err)
	}

	if s.db != nil {
		_, dbErr := s.db.ExecContext(ctx,
			`UPDATE tenants SET stripe_customer_id = $1 WHERE id = $2`,
			c.ID, tenantID)
		if dbErr != nil {
			s.logger.Error("persist stripe_customer_id",
				zap.String("tenant", tenantID), zap.Error(dbErr))
		}
	}

	return c.ID, nil
}

// GetCustomerID looks up the Stripe customer ID for a tenant from the DB.
func (s *StripeService) GetCustomerID(ctx context.Context, tenantID string) (string, error) {
	if s.db == nil {
		return "", fmt.Errorf("no database")
	}
	var cid sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT stripe_customer_id FROM tenants WHERE id = $1`, tenantID).Scan(&cid)
	if err != nil {
		return "", fmt.Errorf("get customer id: %w", err)
	}
	if !cid.Valid || cid.String == "" {
		return "", fmt.Errorf("no stripe customer for tenant %s", tenantID)
	}
	return cid.String, nil
}

// --- Checkout ---

// CreateCheckoutSession creates a Stripe Checkout session for a plan.
func (s *StripeService) CreateCheckoutSession(customerID, planID, successURL, cancelURL string) (string, error) {
	plan, ok := s.plans[planID]
	if !ok {
		return "", fmt.Errorf("unknown plan: %s", planID)
	}

	params := &stripe.CheckoutSessionParams{
		Customer: stripe.String(customerID),
		Mode:     stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(plan.PriceID),
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
	}

	sess, err := checkoutsession.New(params)
	if err != nil {
		return "", fmt.Errorf("create checkout session: %w", err)
	}

	return sess.URL, nil
}

// --- Subscriptions ---

// GetSubscription retrieves the current subscription for a tenant.
func (s *StripeService) GetSubscription(ctx context.Context, tenantID string) (*stripe.Subscription, error) {
	if s.db == nil {
		return nil, fmt.Errorf("no database")
	}

	var subID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT stripe_subscription_id FROM tenants WHERE id = $1`, tenantID).Scan(&subID)
	if err != nil {
		return nil, fmt.Errorf("get subscription id: %w", err)
	}
	if !subID.Valid || subID.String == "" {
		return nil, nil // no subscription
	}

	sub, err := subscription.Get(subID.String, nil)
	if err != nil {
		return nil, fmt.Errorf("get stripe subscription: %w", err)
	}
	return sub, nil
}

// CancelSubscription cancels a tenant's subscription at period end.
func (s *StripeService) CancelSubscription(ctx context.Context, tenantID string) error {
	if s.db == nil {
		return fmt.Errorf("no database")
	}

	var subID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT stripe_subscription_id FROM tenants WHERE id = $1`, tenantID).Scan(&subID)
	if err != nil {
		return fmt.Errorf("get subscription id: %w", err)
	}
	if !subID.Valid || subID.String == "" {
		return fmt.Errorf("no subscription for tenant %s", tenantID)
	}

	_, err = subscription.Cancel(subID.String, nil)
	if err != nil {
		return fmt.Errorf("cancel stripe subscription: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE tenants SET subscription_status = 'canceled' WHERE id = $1`, tenantID)
	if err != nil {
		s.logger.Error("update subscription status after cancel",
			zap.String("tenant", tenantID), zap.Error(err))
	}

	return nil
}

// --- Invoices ---

// GetInvoices retrieves the last N invoices for a tenant's Stripe customer.
func (s *StripeService) GetInvoices(ctx context.Context, tenantID string, limit int) ([]InvoiceRow, error) {
	customerID, err := s.GetCustomerID(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	params := &stripe.InvoiceListParams{
		Customer: stripe.String(customerID),
	}
	params.Filters.AddFilter("limit", "", fmt.Sprintf("%d", limit))

	var rows []InvoiceRow
	iter := invoice.List(params)
	for iter.Next() {
		inv := iter.Invoice()

		status := string(inv.Status)
		statusClass := "default"
		switch inv.Status {
		case stripe.InvoiceStatusPaid:
			statusClass = "success"
		case stripe.InvoiceStatusOpen:
			statusClass = "warning"
		case stripe.InvoiceStatusUncollectible:
			statusClass = "danger"
		}

		rows = append(rows, InvoiceRow{
			ID:          inv.ID,
			Date:        InvoiceDateFmt(inv.Created),
			Amount:      fmt.Sprintf("$%.2f", float64(inv.AmountDue)/100),
			Status:      status,
			StatusClass: statusClass,
			PDFURL:      inv.InvoicePDF,
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("list invoices: %w", err)
	}

	return rows, nil
}

// --- Billing Portal ---

// CreateBillingPortalSession creates a Stripe Billing Portal session
// for self-service subscription management.
func (s *StripeService) CreateBillingPortalSession(ctx context.Context, tenantID, returnURL string) (string, error) {
	customerID, err := s.GetCustomerID(ctx, tenantID)
	if err != nil {
		return "", err
	}

	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(returnURL),
	}

	sess, err := portalsession.New(params)
	if err != nil {
		return "", fmt.Errorf("create billing portal session: %w", err)
	}

	return sess.URL, nil
}

// --- DB Persistence (called from webhook handler) ---

// SaveSubscription persists subscription state to the tenants table.
func (s *StripeService) SaveSubscription(ctx context.Context, tenantID, subscriptionID, status, plan string) error {
	if s.db == nil {
		return fmt.Errorf("no database")
	}

	_, err := s.db.ExecContext(ctx,
		`UPDATE tenants
		 SET stripe_subscription_id = $1,
		     subscription_status = $2,
		     plan = $3
		 WHERE id = $4`,
		subscriptionID, status, plan, tenantID)
	if err != nil {
		return fmt.Errorf("save subscription: %w", err)
	}
	return nil
}

// LookupTenantByCustomer finds the tenant_id for a Stripe customer ID.
func (s *StripeService) LookupTenantByCustomer(ctx context.Context, customerID string) (string, error) {
	if s.db == nil {
		return "", fmt.Errorf("no database")
	}

	var tenantID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM tenants WHERE stripe_customer_id = $1`, customerID).Scan(&tenantID)
	if err != nil {
		return "", fmt.Errorf("lookup tenant by customer %s: %w", customerID, err)
	}
	return tenantID, nil
}
