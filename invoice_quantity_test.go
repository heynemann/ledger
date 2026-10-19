package ledger_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/xraph/ledger/id"
	"github.com/xraph/ledger/meter"
	"github.com/xraph/ledger/types"
)

func TestGenerateInvoiceRejectsQuantityWrapToPositive(t *testing.T) {
	l, store, sub, setNow := windowFixture(t)
	setNow(time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC))

	events := make([]*meter.UsageEvent, 0, 3)
	for _, quantity := range []int64{math.MaxInt64, math.MaxInt64, 3} {
		events = append(events, &meter.UsageEvent{
			ID: id.NewUsageEventID(), TenantID: sub.TenantID, AppID: sub.AppID,
			FeatureKey: "api_calls", Quantity: quantity,
			Timestamp: time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC),
		})
	}

	if err := store.IngestBatch(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	// Unchecked MaxInt64 + MaxInt64 + 3 wraps to 1, silently inside the
	// included allowance. Refuse arithmetic failure before pricing that sum.
	if _, err := l.GenerateInvoice(
		context.Background(),
		sub.ID,
	); !errors.Is(
		err,
		types.ErrOverflow,
	) {
		t.Fatalf("GenerateInvoice error = %v, want quantity overflow", err)
	}
}
