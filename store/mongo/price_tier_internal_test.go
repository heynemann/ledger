package mongo

import (
	"testing"

	"github.com/xraph/ledger/id"
	"github.com/xraph/ledger/plan"
	"github.com/xraph/ledger/types"
)

func TestPlanModelPreservesRationalPriceDenominator(t *testing.T) {
	p := &plan.Plan{
		ID:       id.NewPlanID(),
		Name:     "Tokens",
		Slug:     "tokens",
		Currency: "usd",
		Status:   plan.StatusActive,
		Pricing: &plan.Pricing{
			ID:            id.NewPriceID(),
			BaseAmount:    types.USD(0),
			BillingPeriod: plan.PeriodMonthly,
			Tiers: []plan.PriceTier{
				{
					FeatureKey:   "input_tokens",
					Type:         plan.TierGraduated,
					UnitAmount:   types.USD(9_007_199_254_740_993),
					UnitQuantity: 1_000_000,
				},
			},
		},
	}
	p.Pricing.PlanID = p.ID

	got, err := fromPlanModel(toPlanModel(p))
	if err != nil {
		t.Fatal(err)
	}

	if got.Pricing.Tiers[0].UnitQuantity != 1_000_000 ||
		got.Pricing.Tiers[0].UnitAmount.Amount != 9_007_199_254_740_993 {
		t.Fatalf("rational price changed: %+v", got.Pricing.Tiers[0])
	}
}
