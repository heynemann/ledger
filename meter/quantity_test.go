package meter_test

import (
	"errors"
	"math"
	"testing"

	"github.com/xraph/ledger/meter"
	"github.com/xraph/ledger/types"
)

func TestCheckedQuantitySum(t *testing.T) {
	for _, row := range []struct {
		a, b, want int64
		overflow   bool
	}{
		{0, 0, 0, false},
		{math.MaxInt64, 0, math.MaxInt64, false},
		{math.MaxInt64, 1, 0, true},
		{math.MinInt64, -1, 0, true},
		{math.MaxInt64, -1, math.MaxInt64 - 1, false},
		{math.MinInt64, 1, math.MinInt64 + 1, false},
		{math.MinInt64, math.MaxInt64, -1, false},
	} {
		got, err := meter.CheckedAdd(row.a, row.b)
		if got != row.want || row.overflow != errors.Is(err, types.ErrOverflow) {
			t.Fatalf("%d + %d = %d, %v", row.a, row.b, got, err)
		}
	}
}
