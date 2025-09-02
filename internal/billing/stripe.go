package billing

import (
	"fmt"

	"github.com/stripe/stripe-go/v75"
	"github.com/stripe/stripe-go/v75/checkout/session"
	"github.com/stripe/stripe-go/v75/customer"
)

type StripeService struct {
	apiKey string
}

func NewStripeService(apiKey string) *StripeService {
	stripe.Key = apiKey
	return &StripeService{apiKey: apiKey}
}

func (s *StripeService) CreateCustomer(email, tenantID string) (string, error) {
	params := &stripe.CustomerParams{
		Email: stripe.String(email),
		Metadata: map[string]string{
			"tenant_id": tenantID,
		},
	}

	c, err := customer.New(params)
	if err != nil {
		return "", fmt.Errorf("create customer: %w", err)
	}

	return c.ID, nil
}

// CreateCheckoutSession creates a Stripe Checkout session for $6.69/month subscription
func (s *StripeService) CreateCheckoutSession(customerID, successURL, cancelURL string) (string, error) {
	params := &stripe.CheckoutSessionParams{
		Customer: stripe.String(customerID),
		PaymentMethodTypes: stripe.StringSlice([]string{
			"card",
		}),
		Mode:       stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency: stripe.String("usd"),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name:        stripe.String("stored.ge - 1TB Hybrid Storage"),
						Description: stripe.String("100GB high-performance + 900GB bulk storage"),
					},
					Recurring: &stripe.CheckoutSessionLineItemPriceDataRecurringParams{
						Interval: stripe.String("month"),
					},
					UnitAmount: stripe.Int64(669), // $6.69 in cents
				},
				Quantity: stripe.Int64(1),
			},
		},
	}

	sess, err := session.New(params)
	if err != nil {
		return "", fmt.Errorf("create checkout session: %w", err)
	}

	return sess.URL, nil
}
