package invoice

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/xraph/ledger/plan"
	"github.com/xraph/ledger/types"
)

// UsageChange describes cumulative usage before and after a settlement. Included
// is the native plan allowance (-1 means unlimited); tier bounds count total
// usage, not usage less the allowance.
type UsageChange struct {
	Tiers    []plan.PriceTier
	Before   int64
	After    int64
	Included int64
}

// ComputeIncrementalOverage returns the change in rounded cumulative overage.
// Rational tier charges are summed across all features before rounding each
// cumulative total to nearest, ties to even. Differencing these totals makes
// successive settlements telescope: sub-micro rates are not lost when a usage
// stream is split into small requests. Only the returned difference must fit
// int64; intermediate cumulative amounts may be larger.
func ComputeIncrementalOverage(changes []UsageChange, currency string) (types.Money, error) {
	currency = strings.ToLower(currency)

	var before, after, value big.Rat

	for _, change := range changes {
		if change.Before < 0 || change.After < change.Before || change.Included < -1 {
			return types.Money{}, fmt.Errorf("%w: invalid cumulative usage change", ErrInvalidTiers)
		}

		if validationErr := ValidateTiers(change.Tiers, currency); validationErr != nil {
			return types.Money{}, validationErr
		}

		sorted := orderedTiers(change.Tiers)
		if pricingErr := exactOverage(
			&value,
			sorted,
			change.Before,
			change.Included,
		); pricingErr != nil {
			return types.Money{}, pricingErr
		}

		before.Add(&before, &value)

		if pricingErr := exactOverage(
			&value,
			sorted,
			change.After,
			change.Included,
		); pricingErr != nil {
			return types.Money{}, pricingErr
		}

		after.Add(&after, &value)
	}

	var roundedBefore, roundedAfter big.Int
	roundEven(&roundedBefore, &before)
	roundEven(&roundedAfter, &after)
	roundedAfter.Sub(&roundedAfter, &roundedBefore)

	if !roundedAfter.IsInt64() {
		return types.Money{}, fmt.Errorf("%w: incremental overage", types.ErrOverflow)
	}

	return types.Money{Amount: roundedAfter.Int64(), Currency: currency}, nil
}

// exactGraduated retains fractional rates and wide intermediates until the
// caller has summed tiers and subtracted allowances. Receivers and scratch
// values are reused; arbitrary precision is needed for mixed denominators and
// cumulative totals that exceed int64 while their difference still fits.
func exactGraduated(out *big.Rat, tiers []plan.PriceTier, qty int64) error {
	out.SetInt64(0)

	var (
		consumed int64
		charge   big.Rat
	)

	for i, tier := range tiers {
		if consumed >= qty {
			break
		}

		units := qty - consumed
		if !isUnbounded(tier.UpTo) && i != len(tiers)-1 && tier.UpTo-consumed < units {
			units = tier.UpTo - consumed
		}

		if units <= 0 {
			continue
		}

		if rateErr := exactUnitCharge(&charge, tier, units); rateErr != nil {
			return rateErr
		}

		out.Add(out, &charge)

		consumed += units
	}

	return nil
}

func exactUnitCharge(out *big.Rat, tier plan.PriceTier, qty int64) error {
	denominator := tier.UnitQuantity
	if denominator < 0 {
		return fmt.Errorf("%w: negative unit quantity for %q", ErrInvalidTiers, tier.FeatureKey)
	}

	if denominator == 0 {
		denominator = 1
	}

	out.SetFrac64(tier.UnitAmount.Amount, denominator)

	var quantity big.Rat
	quantity.SetInt64(qty)
	out.Mul(out, &quantity)

	return nil
}

func exactOverage(out *big.Rat, sorted []plan.PriceTier, usage, included int64) error {
	out.SetInt64(0)

	if len(sorted) == 0 || included < 0 || usage <= included {
		return nil
	}

	switch sorted[0].Type {
	case plan.TierGraduated:
		if pricingErr := exactGraduated(out, sorted, usage); pricingErr != nil {
			return pricingErr
		}

		var allowance big.Rat
		if pricingErr := exactGraduated(&allowance, sorted, included); pricingErr != nil {
			return pricingErr
		}

		out.Sub(out, &allowance)
	case plan.TierVolume:
		return exactUnitCharge(out, tierAt(sorted, usage), usage-included)
	case plan.TierFlat:
		out.SetInt64(tierAt(sorted, usage).FlatAmount.Amount)

		if included > 0 {
			var allowance big.Rat
			allowance.SetInt64(tierAt(sorted, included).FlatAmount.Amount)
			out.Sub(out, &allowance)
		}

		if out.Sign() < 0 {
			out.SetInt64(0)
		}
	}

	return nil
}

func roundedMoney(value *big.Rat, currency string) (types.Money, error) {
	var rounded big.Int
	roundEven(&rounded, value)

	if !rounded.IsInt64() {
		return types.Money{}, fmt.Errorf("%w: rounded tier total", types.ErrOverflow)
	}

	return types.Money{Amount: rounded.Int64(), Currency: currency}, nil
}

// roundEven rounds exactly, including negative halfway values, without floats.
func roundEven(out *big.Int, value *big.Rat) {
	var remainder big.Int
	out.QuoRem(value.Num(), value.Denom(), &remainder)
	remainder.Abs(&remainder)
	remainder.Lsh(&remainder, 1)

	comparison := remainder.Cmp(value.Denom())
	if comparison > 0 || comparison == 0 && out.Bit(0) != 0 {
		var unit big.Int
		unit.SetInt64(int64(value.Sign()))
		out.Add(out, &unit)
	}
}
