package client

import (
	"os"
	"strings"

	"github.com/stripe/stripe-go/v80"
)

func stripeBackendConfig(logger leveledLogger) *stripe.BackendConfig {
	config := &stripe.BackendConfig{LeveledLogger: logger}
	if baseURL := strings.TrimRight(os.Getenv("STRIPE_API_BASE_URL"), "/"); baseURL != "" {
		config.URL = stripe.String(baseURL)
	}

	return config
}
