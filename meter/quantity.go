package meter

import (
	"fmt"
	"math"

	"github.com/xraph/ledger/types"
)

// CheckedAdd adds signed meter quantities, including corrections. Overflow
// returns types.ErrOverflow rather than allowing a wrapped total to hide usage.
func CheckedAdd(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("%w: meter quantity %d + %d", types.ErrOverflow, a, b)
	}

	return a + b, nil
}
