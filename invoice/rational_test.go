package invoice_test

import (
	"errors"
	"math"
	"testing"

	"github.com/xraph/ledger/invoice"
	"github.com/xraph/ledger/plan"
	"github.com/xraph/ledger/types"
)

func TestFractionalOverageRoundsOnlyFinalDifference(t *testing.T) {
	for _, row := range []struct {
		name                  string
		tiers                 []plan.PriceTier
		usage, included, want int64
	}{
		{"sum tiers before rounding", []plan.PriceTier{
			{FeatureKey: "tokens", Type: plan.TierGraduated, UpTo: 1, UnitAmount: types.USD(2), UnitQuantity: 5},
			{FeatureKey: "tokens", Type: plan.TierGraduated, UnitAmount: types.USD(2), UnitQuantity: 5},
		}, 2, 0, 1},
		{"subtract exact allowance", []plan.PriceTier{{FeatureKey: "tokens", Type: plan.TierGraduated, UnitAmount: types.USD(1), UnitQuantity: 2}}, 3, 1, 1},
		{"wide product with denominator", []plan.PriceTier{{FeatureKey: "tokens", Type: plan.TierGraduated, UnitAmount: types.USD(math.MaxInt64), UnitQuantity: 2}}, 2, 0, math.MaxInt64},
		{"wide totals with small difference", []plan.PriceTier{{FeatureKey: "tokens", Type: plan.TierGraduated, UnitAmount: types.USD(2)}}, math.MaxInt64, math.MaxInt64 - 1, 2},
		{"volume rational rate", []plan.PriceTier{{FeatureKey: "tokens", Type: plan.TierVolume, UnitAmount: types.USD(3), UnitQuantity: 2}}, 3, 0, 4},
		{"ties even lower", []plan.PriceTier{{FeatureKey: "tokens", Type: plan.TierGraduated, UnitAmount: types.USD(1), UnitQuantity: 2}}, 1, 0, 0},
		{"ties even upper", []plan.PriceTier{{FeatureKey: "tokens", Type: plan.TierGraduated, UnitAmount: types.USD(1), UnitQuantity: 2}}, 3, 0, 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			if err := invoice.ValidateTiers(row.tiers, "usd"); err != nil {
				t.Fatal(err)
			}

			got, err := invoice.ComputeOverage(row.tiers, row.usage, row.included, "USD")
			if err != nil || got.Amount != row.want || got.Currency != "usd" {
				t.Fatalf("overage = %+v, %v; want %d usd micros", got, err, row.want)
			}
		})
	}
}

func TestRationalPricingRejectsInvalidDenominatorAndOverflow(t *testing.T) {
	tiers := []plan.PriceTier{
		{
			FeatureKey:   "tokens",
			Type:         plan.TierGraduated,
			UnitAmount:   types.USD(1),
			UnitQuantity: -1,
		},
	}
	if err := invoice.ValidateTiers(tiers, "usd"); !errors.Is(err, invoice.ErrInvalidTiers) {
		t.Fatalf("negative denominator = %v", err)
	}

	if _, err := invoice.ComputeOverage(
		tiers,
		1,
		0,
		"usd",
	); !errors.Is(
		err,
		invoice.ErrInvalidTiers,
	) {
		t.Fatalf("negative denominator compute = %v", err)
	}

	tiers[0].UnitQuantity = 0

	tiers[0].UnitAmount = types.USD(math.MaxInt64)
	if _, err := invoice.ComputeOverage(tiers, 2, 0, "usd"); !errors.Is(err, types.ErrOverflow) {
		t.Fatalf("money overflow = %v", err)
	}

	tiers[0] = plan.PriceTier{
		FeatureKey:   "tokens",
		Type:         plan.TierFlat,
		FlatAmount:   types.USD(1),
		UnitQuantity: 2,
	}
	if err := invoice.ValidateTiers(tiers, "usd"); !errors.Is(err, invoice.ErrInvalidTiers) {
		t.Fatalf("flat denominator = %v", err)
	}
}

func TestNativeIncrementalOverageRetainsDiscountSemantics(t *testing.T) {
	// Spending disallows decreasing debits, but native invoice callers retain
	// volume discounts and signed differences with exact, symmetric rounding.
	tiers := []plan.PriceTier{
		{
			FeatureKey:   "tokens",
			Type:         plan.TierVolume,
			UpTo:         1,
			UnitAmount:   types.USD(7),
			UnitQuantity: 2,
		},
		{FeatureKey: "tokens", Type: plan.TierVolume, UnitAmount: types.USD(1), UnitQuantity: 2},
	}

	got, err := invoice.ComputeIncrementalOverage(
		[]invoice.UsageChange{{Tiers: tiers, Before: 1, After: 2}},
		"usd",
	)
	if err != nil || got.Amount != -3 {
		t.Fatalf("signed increment = %+v, %v", got, err)
	}

	if _, err := invoice.ComputeIncrementalOverage(
		[]invoice.UsageChange{{Tiers: tiers, Before: 2, After: 1}},
		"usd",
	); err == nil {
		t.Fatal("decreasing quantity accepted")
	}
}

func BenchmarkComputeOverageFractional(b *testing.B) {
	tiers := []plan.PriceTier{
		{
			FeatureKey:   "tokens",
			Type:         plan.TierGraduated,
			UnitAmount:   types.USD(3),
			UnitQuantity: 1_000_000,
		},
	}

	b.ReportAllocs()

	for b.Loop() {
		if _, err := invoice.ComputeOverage(tiers, 5_000_000, 1_000_000, "usd"); err != nil {
			b.Fatal(err)
		}
	}
}
