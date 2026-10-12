package util

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrAttemptBudgetExhausted reports that a logical request has consumed every
// allowed upstream HTTP attempt.
var ErrAttemptBudgetExhausted = errors.New("upstream attempt budget exhausted")

type attemptBudgetKey struct{}

type attemptBudget struct {
	max  int64
	used atomic.Int64
}

// WithAttemptBudget attaches a shared physical-request budget unless ctx already
// carries one. Nested retry layers therefore cooperate instead of multiplying.
func WithAttemptBudget(ctx context.Context, max int) context.Context {
	if ctx == nil || max < 1 {
		return ctx
	}
	if ctx.Value(attemptBudgetKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, attemptBudgetKey{}, &attemptBudget{max: int64(max)})
}

// ConsumeAttempt reserves one upstream HTTP attempt. attempt is one-based and
// max is the request-wide ceiling. ok is false after the budget is exhausted.
// Contexts without a budget remain unrestricted.
func ConsumeAttempt(ctx context.Context) (attempt, max int, ok bool) {
	if ctx == nil {
		return 0, 0, true
	}
	budget, _ := ctx.Value(attemptBudgetKey{}).(*attemptBudget)
	if budget == nil {
		return 0, 0, true
	}
	used := budget.used.Add(1)
	if used > budget.max {
		return int(used), int(budget.max), false
	}
	return int(used), int(budget.max), true
}

// AttemptsRemaining reports whether a shared budget can issue another physical
// request. Contexts without a budget always return true.
func AttemptsRemaining(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	budget, _ := ctx.Value(attemptBudgetKey{}).(*attemptBudget)
	return budget == nil || budget.used.Load() < budget.max
}
