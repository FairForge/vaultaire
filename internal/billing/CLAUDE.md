# internal/billing

Stripe billing integration for stored.ge subscriptions, payments, and invoices.

## Key Types

- **StripeService** — manages Stripe customers, checkout sessions, subscriptions, invoices, and billing portal. Holds `*sql.DB` for persisting Stripe IDs to the `tenants` table.
- **WebhookHandler** — `http.Handler` for `POST /webhook/stripe`. Verifies signatures, processes events, updates DB.
- **Plan** — maps an internal plan ID to a Stripe Price ID with display metadata.
- **InvoiceRow** — formatted invoice data for dashboard templates.
- **OverageService** — tracks grace periods for tenants exceeding quotas.

## StripeService Methods

| Method | Purpose |
|--------|---------|
| `CreateCustomer(ctx, email, tenantID)` | Create Stripe customer, persist `stripe_customer_id` |
| `GetCustomerID(ctx, tenantID)` | Look up Stripe customer ID from DB |
| `CreateCheckoutSession(customerID, planID, successURL, cancelURL)` | Create checkout for a registered plan |
| `GetSubscription(ctx, tenantID)` | Fetch subscription from Stripe |
| `CancelSubscription(ctx, tenantID)` | Cancel at period end, update DB status |
| `GetInvoices(ctx, tenantID, limit)` | List recent invoices from Stripe |
| `CreateBillingPortalSession(ctx, tenantID, returnURL)` | Self-service billing portal |
| `SaveSubscription(ctx, tenantID, subID, status, plan)` | Persist subscription state (called by webhook) |
| `LookupTenantByCustomer(ctx, customerID)` | Reverse lookup tenant from Stripe customer ID |
| `RegisterPlan(plan)` | Register a plan with Stripe Price ID at startup |

## Webhook Events Handled

| Event | Action |
|-------|--------|
| `checkout.session.completed` | Activate subscription, save to DB |
| `invoice.payment_succeeded` | Mark subscription active |
| `invoice.payment_failed` | Mark subscription past_due |
| `customer.subscription.updated` | Sync status and plan |
| `customer.subscription.deleted` | Downgrade to starter, clear subscription |

## Environment Variables (not yet wired in main.go)

- `STRIPE_SECRET_KEY` — Stripe API key
- `STRIPE_WEBHOOK_SECRET` — webhook endpoint secret (whsec_...)

## Testing

- Unit tests: `go test ./internal/billing/... -short` (no Stripe key needed)
- Integration tests: set `STRIPE_TEST_KEY` env var + `stripe listen --forward-to localhost:8000/webhook/stripe`

## Next Phases

- **2.2**: Wire webhook route in server.go
- **2.3**: Stripe event idempotency (migration 019)
- **2.4**: Registration → auto-create Stripe customer
- **2.5**: Billing dashboard page
- **2.6**: Full server wiring with env vars
