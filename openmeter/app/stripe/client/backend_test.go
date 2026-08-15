package client

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripeBackendConfigUsesSimulationBaseURL(t *testing.T) {
	t.Setenv("STRIPE_API_BASE_URL", "http://stripe-sim:8080/")

	config := stripeBackendConfig(leveledLogger{})

	require.NotNil(t, config.URL)
	require.Equal(t, "http://stripe-sim:8080", *config.URL)
}

func TestStripeBackendConfigKeepsProductionDefault(t *testing.T) {
	t.Setenv("STRIPE_API_BASE_URL", "")

	config := stripeBackendConfig(leveledLogger{})

	require.Nil(t, config.URL)
}
