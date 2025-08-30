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

type CheckoutSession struct {
	ID  string
	URL string
}

func (s *StripeService) CreateCheckoutSession(customerID, priceID, successURL string) (*CheckoutSession, error) {
	params := &stripe.CheckoutSessionParams{
		Customer: stripe.String(customerID),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(1),
			},
		},
		Mode:       stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		SuccessURL: stripe.String(successURL),
	}

	sess, err := session.New(params)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	return &CheckoutSession{
		ID:  sess.ID,
		URL: sess.URL,
	}, nil
}
