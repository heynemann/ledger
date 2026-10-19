package invoice

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/xraph/ledger/plan"
	"github.com/xraph/ledger/types"
)

// isUnbounded reports whether an UpTo value marks a tier as unbounded.
// The schema's own zero value means unbounded, but the repository's public
// documentation (README.md, docs/API.md, docs_test.go, and five pages under
// docs/content) uses -1. Both, and anything else non-positive, mean the
// same thing: this tier has no ceiling.
func isUnbounded(upTo int64) bool {
	return upTo <= 0
}

// SortTiers returns tiers ordered for evaluation: by UpTo ascending, with
// any UpTo <= 0 (0 in this package's own tests, -1 in the documented
// schema) treated as unbounded and sorted last, and Priority breaking ties.
//
// The input is not mutated. Callers hold tiers that belong to a stored
// plan, and reordering that slice in place would rewrite the plan's own
// view of its pricing.
func SortTiers(tiers []plan.PriceTier) []plan.PriceTier {
	out := make([]plan.PriceTier, len(tiers))
	copy(out, tiers)

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]

		aUnbounded, bUnbounded := isUnbounded(a.UpTo), isUnbounded(b.UpTo)
		switch {
		case aUnbounded && bUnbounded:
			return a.Priority < b.Priority
		case aUnbounded:
			return false
		case bUnbounded:
			return true
		case a.UpTo != b.UpTo:
			return a.UpTo < b.UpTo
		default:
			return a.Priority < b.Priority
		}
	})

	return out
}

// orderedTiers borrows an already trivially ordered ladder; callers only read
// it. Public SortTiers still returns independent caller-owned storage.
func orderedTiers(tiers []plan.PriceTier) []plan.PriceTier {
	if len(tiers) < 2 {
		return tiers
	}

	return SortTiers(tiers)
}

// tierAt returns the tier that a total quantity of qty falls into. The last
// tier in a sorted ladder always matches, even when it is not marked
// unbounded: a ladder with no unbounded tier does not stop pricing at its
// highest UpTo, it extends its last tier to cover everything above it.
//
// tiers must already be sorted by SortTiers and non-empty; an empty slice
// returns the zero plan.PriceTier rather than panicking, since every caller
// in this file already guards on len(tiers) == 0 before reaching here.
func tierAt(tiers []plan.PriceTier, qty int64) plan.PriceTier {
	for i, t := range tiers {
		if isUnbounded(t.UpTo) || i == len(tiers)-1 || qty <= t.UpTo {
			return t
		}
	}

	return plan.PriceTier{}
}

// ComputeOverage prices the billable excess of usage over an included
// allowance, using whichever tier model the plan declares.
//
// currency is normalised to lowercase once, here, at entry. Every Money
// this function and its helpers construct downstream uses that normalised
// value, so a caller passing "USD" gets an amount back in "usd" rather than
// hitting the currency-mismatch panic in types.Money.Add/Subtract.
//
// UpTo counts TOTAL period usage, not units past the allowance: a plan that
// prices its allowance as a $0 first tier (as this package's README example
// does) needs `usage`, not `usage - included`, to land in the right tier,
// or the allowance would be priced twice. Concretely:
//   - graduated: price(usage) - price(included). Each unit is charged at
//     the rate of the tier position it occupies in the ladder; billing only
//     the delta between the two totals prices the included units once, at
//     graduated's per-position rate, not again on top of it.
//   - volume: the tier reached by TOTAL usage sets the rate, applied to
//     usage - included units.
//   - flat: max(0, fee(usage) - fee(included)), where fee(q) is the
//     FlatAmount of the tier reached by total quantity q. Charging the
//     whole band fee for any usage past included would re-bill the band
//     the allowance already sat in: included 3000 with usage 3001 must not
//     bill a fresh fee for one unit inside a band already paid for.
//
// A ladder with no unbounded tier does not stop pricing at its highest
// UpTo: the last tier after sorting extends to cover everything above it.
//
// Rational rates are preserved through tier summation and allowance subtraction.
// The returned monetary total is rounded once to nearest, ties to even. An
// amount outside int64 returns an error wrapping types.ErrOverflow, never a
// wrapped amount. An intermediate product may exceed int64 when the final
// allowance difference fits.
//
// included < 0 means an unlimited allowance, matching plan.Feature.Limit ==
// -1, and prices at zero regardless of usage. usage <= included also prices
// at zero.
//
// An empty tier slice prices at zero: a metered feature whose plan declares
// no tiers is a gap in the catalogue, not a billing failure. Callers must
// call ValidateTiers before ComputeOverage; invalid ladders are not a pricing
// contract. Negative denominators are rejected rather than divided.
func ComputeOverage(
	tiers []plan.PriceTier,
	usage, included int64,
	currency string,
) (types.Money, error) {
	currency = strings.ToLower(currency)

	var total big.Rat

	if len(tiers) == 0 || included < 0 || usage <= included {
		return types.Zero(currency), nil
	}

	sorted := orderedTiers(tiers)
	if pricingErr := exactOverage(&total, sorted, usage, included); pricingErr != nil {
		return types.Money{}, pricingErr
	}

	amount, roundingErr := roundedMoney(&total, currency)
	if roundingErr != nil {
		return types.Money{}, fmt.Errorf(
			"%s pricing for feature %q: %w",
			sorted[0].Type,
			sorted[0].FeatureKey,
			roundingErr,
		)
	}

	return amount, nil
}

// ErrInvalidTiers is wrapped by every ValidateTiers failure.
var ErrInvalidTiers = errors.New("invoice: invalid price tiers")

// ValidateTiers reports whether one feature's tiers can be priced
// unambiguously in the given currency. Callers must validate before
// ComputeOverage, which prices an invalid ladder by best effort and reports
// only arithmetic overflow.
//
// Every returned error names the offending tier by its index in the slice
// and its UpTo, not only its FeatureKey: a plan with several tiers sharing
// one feature key would otherwise leave the caller guessing which tier is
// bad.
//
// An empty slice is valid: a metered feature with no tiers yet is a
// catalogue gap for ComputeOverage to price at zero, not a validation
// failure. Otherwise ValidateTiers rejects a ladder where:
//  1. the tiers do not all share one Type;
//  2. any Type is empty or not one of graduated, volume, or flat;
//  3. any tier's UnitAmount.Currency or FlatAmount.Currency is non-empty
//     and differs from currency, case-insensitively;
//  4. the tiers carry more than one distinct FeatureKey;
//  5. two tiers have the same UpTo, treating every UpTo <= 0 as the same
//     unbounded value (so a 0 and a -1 together are a duplicate);
//  6. a graduated or volume tier has a non-zero FlatAmount, or a flat tier
//     has a non-zero UnitAmount;
//  7. any tier's UnitAmount.Amount or FlatAmount.Amount is negative. A
//     negative rate would quietly reduce an invoice instead of erroring.
func ValidateTiers(tiers []plan.PriceTier, currency string) error {
	if len(tiers) == 0 {
		return nil
	}

	currency = strings.ToLower(currency)
	wantType := tiers[0].Type
	wantFeatureKey := tiers[0].FeatureKey
	seenUpTo := make(map[int64]bool, len(tiers))

	for idx, t := range tiers {
		if t.UnitQuantity < 0 || t.Type == plan.TierFlat && t.UnitQuantity != 0 {
			return fmt.Errorf("%w: tier %d (feature %q) has invalid unit quantity %d",
				ErrInvalidTiers, idx, t.FeatureKey, t.UnitQuantity)
		}

		if t.Type != wantType {
			return fmt.Errorf("%w: tier %d (UpTo %d, feature %q) mixes types %q and %q",
				ErrInvalidTiers, idx, t.UpTo, t.FeatureKey, wantType, t.Type)
		}

		switch t.Type {
		case plan.TierGraduated, plan.TierVolume, plan.TierFlat:
		default:
			return fmt.Errorf("%w: tier %d (UpTo %d, feature %q) has invalid type %q",
				ErrInvalidTiers, idx, t.UpTo, t.FeatureKey, t.Type)
		}

		if t.UnitAmount.Currency != "" && !strings.EqualFold(t.UnitAmount.Currency, currency) {
			return fmt.Errorf(
				"%w: tier %d (UpTo %d, feature %q) unit amount currency %q does not match %q",
				ErrInvalidTiers,
				idx,
				t.UpTo,
				t.FeatureKey,
				t.UnitAmount.Currency,
				currency,
			)
		}

		if t.FlatAmount.Currency != "" && !strings.EqualFold(t.FlatAmount.Currency, currency) {
			return fmt.Errorf(
				"%w: tier %d (UpTo %d, feature %q) flat amount currency %q does not match %q",
				ErrInvalidTiers,
				idx,
				t.UpTo,
				t.FeatureKey,
				t.FlatAmount.Currency,
				currency,
			)
		}

		if t.UnitAmount.Amount < 0 {
			return fmt.Errorf("%w: tier %d (UpTo %d, feature %q) has a negative unit amount %d",
				ErrInvalidTiers, idx, t.UpTo, t.FeatureKey, t.UnitAmount.Amount)
		}

		if t.FlatAmount.Amount < 0 {
			return fmt.Errorf("%w: tier %d (UpTo %d, feature %q) has a negative flat amount %d",
				ErrInvalidTiers, idx, t.UpTo, t.FeatureKey, t.FlatAmount.Amount)
		}

		if t.FeatureKey != wantFeatureKey {
			return fmt.Errorf("%w: tier %d (UpTo %d, feature %q) does not match feature key %q",
				ErrInvalidTiers, idx, t.UpTo, t.FeatureKey, wantFeatureKey)
		}

		upToKey := t.UpTo
		if isUnbounded(upToKey) {
			upToKey = 0 // every non-positive UpTo shares one bucket
		}

		if seenUpTo[upToKey] {
			return fmt.Errorf("%w: tier %d (UpTo %d, feature %q) duplicates another tier's UpTo",
				ErrInvalidTiers, idx, t.UpTo, t.FeatureKey)
		}

		seenUpTo[upToKey] = true

		switch t.Type {
		case plan.TierGraduated, plan.TierVolume:
			if t.FlatAmount.Amount != 0 {
				return fmt.Errorf(
					"%w: tier %d (UpTo %d, feature %q) is %s but carries a non-zero flat amount",
					ErrInvalidTiers,
					idx,
					t.UpTo,
					t.FeatureKey,
					t.Type,
				)
			}
		case plan.TierFlat:
			if t.UnitAmount.Amount != 0 {
				return fmt.Errorf(
					"%w: tier %d (UpTo %d, feature %q) is flat but carries a non-zero unit amount",
					ErrInvalidTiers,
					idx,
					t.UpTo,
					t.FeatureKey,
				)
			}
		}
	}

	return nil
}
