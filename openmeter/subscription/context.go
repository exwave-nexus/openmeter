package subscription

import (
	"context"
	"time"

	"github.com/openmeterio/openmeter/pkg/models"
)

type contextKey string

const (
	subscriptionoperation contextKey = "subscriptionoperation"
	planreplacement       contextKey = "planreplacement"
)

type planReplacement struct {
	SubscriptionID models.NamespacedID
	EffectiveAt    time.Time
}

// WithPlanReplacementTime scopes the workflow's validated current-period
// replacement time to one subscription. Ordinary cancellation has no override.
func WithPlanReplacementTime(ctx context.Context, id models.NamespacedID, at time.Time) context.Context {
	return context.WithValue(ctx, planreplacement, planReplacement{SubscriptionID: id, EffectiveAt: at})
}

func PlanReplacementTime(ctx context.Context, id models.NamespacedID) (time.Time, bool) {
	replacement, ok := ctx.Value(planreplacement).(planReplacement)
	return replacement.EffectiveAt, ok && replacement.SubscriptionID == id
}

func NewSubscriptionOperationContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, subscriptionoperation, true)
}

func IsSubscriptionOperation(ctx context.Context) bool {
	u, ok := ctx.Value(subscriptionoperation).(bool)
	if !ok {
		return false
	}

	return u
}
