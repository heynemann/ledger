package spending_test

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xraph/ledger/plan"
	"github.com/xraph/ledger/spending"
	"github.com/xraph/ledger/types"
)

func pricedPlan() *plan.Plan {
	return &plan.Plan{
		Name:     "Tokens",
		Slug:     "tokens",
		Currency: "usd",
		Status:   plan.StatusActive,
		Features: []plan.Feature{
			{Key: "input_tokens", Type: plan.FeatureMetered, Limit: 0, Period: plan.PeriodMonthly},
		},
		Pricing: &plan.Pricing{
			BaseAmount:    types.USD(0),
			BillingPeriod: plan.PeriodNone,
			Tiers: []plan.PriceTier{
				{
					FeatureKey:   "input_tokens",
					Type:         plan.TierGraduated,
					UnitAmount:   types.USD(1),
					UnitQuantity: 1_000_000,
				},
			},
		},
	}
}

func requireRate(
	t *testing.T,
	p *plan.Plan,
	before map[string]spending.MeterTotal,
	delta map[string]*int64,
	want *int64,
) {
	t.Helper()

	got, err := spending.New().Rate(p, before, delta)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Rate = %v, want %v (nil means unknown)", got, want)
	}
}

func TestRateFractionalMillionAndTiesEven(t *testing.T) {
	p := pricedPlan()
	for _, row := range []struct{ before, delta, want int64 }{
		{0, 499_999, 0},
		{0, 500_000, 0},
		{0, 500_001, 1},
		{0, 1_500_000, 2},
		{0, 2_500_000, 2},
		{500_000, 1, 1},
		{500_001, 499_999, 0},
	} {
		requireRate(
			t,
			p,
			map[string]spending.MeterTotal{"input_tokens": {Quantity: row.before}},
			map[string]*int64{"input_tokens": new(row.delta)},
			new(row.want),
		)
	}
}

func TestRateSmallSettlementsTelescope(t *testing.T) {
	p := pricedPlan()
	p.Pricing.Tiers[0].UnitQuantity = 2

	var sum int64

	for before, want := range []int64{0, 1, 1, 0} {
		amount, err := spending.New().
			Rate(p, map[string]spending.MeterTotal{"input_tokens": {Quantity: int64(before)}}, map[string]*int64{"input_tokens": new(int64(1))})
		if err != nil || amount == nil || *amount != want {
			t.Fatalf("settlement %d: %v, %v; want %d", before, amount, err, want)
		}

		sum += *amount
	}

	if sum != 2 {
		t.Fatalf("four half-micro units charged %d, want 2", sum)
	}
}

func TestRateRoundsAcrossMetersNotEachFeature(t *testing.T) {
	p := pricedPlan()
	p.Features = append(
		p.Features,
		plan.Feature{
			Key:    "output_tokens",
			Type:   plan.FeatureMetered,
			Limit:  0,
			Period: plan.PeriodMonthly,
		},
	)
	p.Pricing.Tiers[0].UnitAmount = types.USD(2)
	p.Pricing.Tiers[0].UnitQuantity = 5
	tier := p.Pricing.Tiers[0]
	tier.FeatureKey = "output_tokens"
	p.Pricing.Tiers = append(p.Pricing.Tiers, tier)
	requireRate(
		t,
		p,
		map[string]spending.MeterTotal{"input_tokens": {}, "output_tokens": {}},
		map[string]*int64{"input_tokens": new(int64(1)), "output_tokens": new(int64(1))},
		new(int64(1)),
	)
}

func TestRateIncludedAllowanceAndIncrementalTiers(t *testing.T) {
	p := pricedPlan()
	p.Features[0].Limit = 8
	p.Pricing.Tiers = []plan.PriceTier{
		{
			FeatureKey:   "input_tokens",
			Type:         plan.TierGraduated,
			UpTo:         10,
			UnitAmount:   types.USD(1),
			UnitQuantity: 2,
		},
		{
			FeatureKey:   "input_tokens",
			Type:         plan.TierGraduated,
			UnitAmount:   types.USD(3),
			UnitQuantity: 2,
		},
	}
	requireRate(
		t,
		p,
		map[string]spending.MeterTotal{"input_tokens": {Quantity: 9}},
		map[string]*int64{"input_tokens": new(int64(2))},
		new(int64(2)),
	)
	// Below the included allowance, usage is known free even with a base fee.
	p.Pricing.BaseAmount = types.USD(123_000_000)
	requireRate(
		t,
		p,
		map[string]spending.MeterTotal{"input_tokens": {}},
		map[string]*int64{"input_tokens": new(int64(8))},
		new(int64(0)),
	)
}

func TestRateProviderCostMarkup(t *testing.T) {
	p := pricedPlan()
	p.Features[0].Key = "provider_cost_micros"
	p.Pricing.Tiers[0].FeatureKey = "provider_cost_micros"
	p.Pricing.Tiers[0].UnitAmount = types.USD(12_000)
	p.Pricing.Tiers[0].UnitQuantity = 10_000
	requireRate(
		t,
		p,
		map[string]spending.MeterTotal{"provider_cost_micros": {Quantity: 10}},
		map[string]*int64{"provider_cost_micros": new(int64(5))},
		new(int64(6)),
	)
}

func TestRateUnknownIsNotZero(t *testing.T) {
	p := pricedPlan()

	for _, row := range []struct {
		name   string
		before map[string]spending.MeterTotal
		delta  map[string]*int64
	}{
		{"missing prior", nil, map[string]*int64{"input_tokens": new(int64(1))}},
		{"missing delta", map[string]spending.MeterTotal{"input_tokens": {}}, nil},
		{"unknown delta", map[string]spending.MeterTotal{"input_tokens": {}}, map[string]*int64{"input_tokens": nil}},
		{"unknown history", map[string]spending.MeterTotal{"input_tokens": {Unknown: 1}}, map[string]*int64{"input_tokens": new(int64(0))}},
	} {
		t.Run(row.name, func(t *testing.T) { requireRate(t, p, row.before, row.delta, nil) })
	}

	requireRate(
		t,
		p,
		map[string]spending.MeterTotal{"input_tokens": {}, "output_tokens": {Unknown: 1}},
		map[string]*int64{"input_tokens": new(int64(0)), "output_tokens": nil},
		new(int64(0)),
	)
	p.Features[0].Limit = -1
	requireRate(t, p, nil, nil, new(int64(0)))
	p.Features[0].Limit = 0
	p.Pricing.Tiers = nil
	requireRate(t, p, nil, nil, new(int64(0)))
	p.Pricing = nil
	requireRate(t, p, nil, nil, new(int64(0)))
}

func TestRateRejectsNegativeAndOverflowInputs(t *testing.T) {
	for _, row := range []struct {
		name     string
		before   spending.MeterTotal
		delta    int64
		overflow bool
	}{
		{"negative prior", spending.MeterTotal{Quantity: -1}, 0, false},
		{"negative unknown count", spending.MeterTotal{Unknown: -1}, 0, false},
		{"negative delta", spending.MeterTotal{}, -1, false},
		{"quantity overflow", spending.MeterTotal{Quantity: math.MaxInt64}, 1, true},
		{"unknown does not hide overflow", spending.MeterTotal{Quantity: math.MaxInt64, Unknown: 1}, 1, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			amount, err := spending.New().
				Rate(pricedPlan(), map[string]spending.MeterTotal{"input_tokens": row.before}, map[string]*int64{"input_tokens": new(row.delta)})
			if err == nil || amount != nil || row.overflow && !errors.Is(err, types.ErrOverflow) {
				t.Fatalf("Rate = %v, %v", amount, err)
			}
		})
	}

	p := pricedPlan()
	p.Pricing.Tiers[0].UnitAmount = types.USD(math.MaxInt64)
	p.Pricing.Tiers[0].UnitQuantity = 0

	amount, err := spending.New().
		Rate(p, map[string]spending.MeterTotal{"input_tokens": {}}, map[string]*int64{"input_tokens": new(int64(2))})
	if amount != nil || !errors.Is(err, types.ErrOverflow) {
		t.Fatalf("monetary overflow = %v, %v", amount, err)
	}
	// Large cumulative money can cancel exactly to a small valid increment.
	p.Pricing.Tiers[0].UnitAmount = types.USD(2)
	requireRate(
		t,
		p,
		map[string]spending.MeterTotal{"input_tokens": {Quantity: math.MaxInt64 - 1}},
		map[string]*int64{"input_tokens": new(int64(1))},
		new(int64(2)),
	)
}

func TestValidatePlanReusesNativeRulesAndSpendingConstraints(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func(*plan.Plan)
	}{
		{"missing name", func(p *plan.Plan) { p.Name = "" }},
		{"duplicate feature", func(p *plan.Plan) { p.Features = append(p.Features, p.Features[0]) }},
		{"unknown feature type", func(p *plan.Plan) { p.Features[0].Type = "unknown" }},
		{"invalid allowance", func(p *plan.Plan) { p.Features[0].Limit = -2 }},
		{"missing meter period", func(p *plan.Plan) { p.Features[0].Period = "" }},
		{"non USD", func(p *plan.Plan) { p.Currency = "eur"; p.Pricing.BaseAmount = types.EUR(0) }},
		{"negative denominator", func(p *plan.Plan) { p.Pricing.Tiers[0].UnitQuantity = -1 }},
		{"orphan tier", func(p *plan.Plan) { p.Pricing.Tiers[0].FeatureKey = "missing" }},
		{"self referential charge", func(p *plan.Plan) {
			p.Features[0].Key = "customer_charge_micros"
			p.Pricing.Tiers[0].FeatureKey = p.Features[0].Key
		}},
		{"seat pricing", func(p *plan.Plan) { p.Features[0].Type = plan.FeatureSeat }},
		{"unsupported billing period", func(p *plan.Plan) { p.Pricing.BillingPeriod = "weekly" }},
		{"plugin strategy", func(p *plan.Plan) { p.Metadata = map[string]string{"pricing_strategy": "plugin"} }},
		{"plugin aggregator", func(p *plan.Plan) { p.Features[0].Metadata = map[string]string{"aggregator": "plugin"} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := pricedPlan()
			row.change(p)

			if err := spending.New().ValidatePlan(p); err == nil {
				t.Fatal("invalid plan accepted")
			}
		})
	}

	if err := spending.New().ValidatePlan(nil); err == nil {
		t.Fatal("nil plan accepted")
	}
}

func TestSpendingRejectsDecreasingDebits(t *testing.T) {
	for _, model := range []plan.TierType{plan.TierVolume, plan.TierFlat} {
		p := pricedPlan()

		p.Pricing.Tiers = []plan.PriceTier{
			{FeatureKey: "input_tokens", Type: model, UpTo: 10},
			{FeatureKey: "input_tokens", Type: model},
		}
		if model == plan.TierVolume {
			p.Pricing.Tiers[0].UnitAmount, p.Pricing.Tiers[0].UnitQuantity = types.USD(2), 3
			p.Pricing.Tiers[1].UnitAmount, p.Pricing.Tiers[1].UnitQuantity = types.USD(3), 5
		} else {
			p.Pricing.Tiers[0].FlatAmount = types.USD(2)
			p.Pricing.Tiers[1].FlatAmount = types.USD(1)
		}

		if err := spending.New().ValidatePlan(p); err == nil {
			t.Fatalf("accepted decreasing %s schedule", model)
		}

		if model == plan.TierVolume {
			p.Pricing.Tiers[1].UnitAmount = types.USD(4)
		} else {
			p.Pricing.Tiers[1].FlatAmount = types.USD(3)
		}

		if err := spending.New().ValidatePlan(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEngineDoesNotMutateInputsOrCatalog(t *testing.T) {
	p := pricedPlan()

	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}

	before := map[string]spending.MeterTotal{"input_tokens": {}}
	delta := map[string]*int64{"input_tokens": new(int64(1))}
	requireRate(t, p, before, delta, new(int64(0)))

	after, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}

	if string(encoded) != string(after) || before["input_tokens"].Quantity != 0 ||
		*delta["input_tokens"] != 1 {
		t.Fatal("Rate mutated its inputs")
	}

	catalog := spending.BuiltinMeters()
	if len(catalog) != 8 {
		t.Fatalf("catalog length %d", len(catalog))
	}

	catalog[0].Key = "corrupt"
	if spending.BuiltinMeters()[0].Key != "requests" {
		t.Fatal("catalog storage is shared")
	}

	for _, item := range spending.BuiltinMeters() {
		if item.Key == "customer_charge_micros" {
			t.Fatal("self-referential retail meter")
		}
	}
}

func TestCalendarWindowsUTCAndHalfOpen(t *testing.T) {
	engine := spending.New()

	at := time.Date(2024, 3, 1, 0, 30, 0, 0, time.FixedZone("east", 2*3600))
	for _, row := range []struct {
		period     string
		start, end time.Time
	}{
		{"day", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"month", time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
	} {
		window, err := engine.Window(row.period, at)
		if err != nil || !window.Start.Equal(row.start) || window.End == nil ||
			!window.End.Equal(row.end) ||
			window.Start.Location() != time.UTC {
			t.Fatalf("%s = %+v, %v", row.period, window, err)
		}

		next, err := engine.Window(row.period, *window.End)
		if err != nil || !next.Start.Equal(*window.End) {
			t.Fatalf("adjacent %s windows overlap or gap", row.period)
		}
	}

	lifetime, err := engine.Window("lifetime", at)
	if err != nil || !lifetime.Start.Equal(time.Unix(0, 0)) || lifetime.End != nil {
		t.Fatalf("lifetime = %+v, %v", lifetime, err)
	}

	if _, err := engine.Window("year", at); err == nil {
		t.Fatal("accepted unsupported period")
	}
}

func TestSettledBudgetDecisions(t *testing.T) {
	engine := spending.New()

	budget := spending.Budget{
		Name:        "Monthly spend",
		Basis:       "provider_cost",
		Period:      "month",
		LimitMicros: 10,
		Enabled:     true,
	}
	for _, row := range []struct {
		settled int64
		want    bool
	}{{0, false}, {9, false}, {10, true}, {11, true}, {math.MaxInt64, true}} {
		got, err := engine.Exceeded(budget, row.settled)
		if err != nil || got != row.want {
			t.Fatalf("settled %d = %v, %v", row.settled, got, err)
		}
	}

	budget.LimitMicros = 0
	if got, err := engine.Exceeded(budget, 0); err != nil || !got {
		t.Fatalf("zero limit = %v, %v", got, err)
	}

	budget.Enabled = false
	if got, err := engine.Exceeded(budget, math.MaxInt64); err != nil || got {
		t.Fatalf("disabled = %v, %v", got, err)
	}

	if _, err := engine.Exceeded(budget, -1); err == nil {
		t.Fatal("negative settled amount accepted")
	}

	for _, mutate := range []func(*spending.Budget){
		func(b *spending.Budget) { b.Basis = "retail" }, func(b *spending.Budget) { b.Period = "year" },
		func(b *spending.Budget) { b.Name = " " }, func(b *spending.Budget) { b.Name = strings.Repeat("x", 256) },
		func(b *spending.Budget) { b.LimitMicros = -1 }, func(b *spending.Budget) { b.Revision = -1 },
	} {
		invalid := budget
		mutate(&invalid)

		if err := engine.ValidateBudget(invalid); err == nil {
			t.Fatalf("invalid budget accepted: %+v", invalid)
		}
	}

	budget.Basis = "customer_charge"

	budget.Period = "lifetime"
	if err := engine.ValidateBudget(budget); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkRateFractional(b *testing.B) {
	engine := spending.New()
	p := pricedPlan()
	before := map[string]spending.MeterTotal{"input_tokens": {Quantity: 1_000_000}}
	delta := map[string]*int64{"input_tokens": new(int64(1_000))}

	b.ReportAllocs()

	for b.Loop() {
		if _, err := engine.Rate(p, before, delta); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRateNativeVolumeAndFlatBoundaries(t *testing.T) {
	p := pricedPlan()
	p.Pricing.Tiers = []plan.PriceTier{
		{
			FeatureKey:   "input_tokens",
			Type:         plan.TierVolume,
			UpTo:         10,
			UnitAmount:   types.USD(1),
			UnitQuantity: 2,
		},
		{FeatureKey: "input_tokens", Type: plan.TierVolume, UnitAmount: types.USD(1)},
	}
	requireRate(
		t,
		p,
		map[string]spending.MeterTotal{"input_tokens": {Quantity: 10}},
		map[string]*int64{"input_tokens": new(int64(1))},
		new(int64(6)),
	)
	p.Pricing.Tiers = []plan.PriceTier{
		{FeatureKey: "input_tokens", Type: plan.TierFlat, UpTo: 10, FlatAmount: types.USD(5)},
		{FeatureKey: "input_tokens", Type: plan.TierFlat, FlatAmount: types.USD(8)},
	}
	requireRate(
		t,
		p,
		map[string]spending.MeterTotal{"input_tokens": {Quantity: 9}},
		map[string]*int64{"input_tokens": new(int64(1))},
		new(int64(0)),
	)
	requireRate(
		t,
		p,
		map[string]spending.MeterTotal{"input_tokens": {Quantity: 10}},
		map[string]*int64{"input_tokens": new(int64(1))},
		new(int64(3)),
	)
}

func TestSpendingJSONPreservesFullInt64Amounts(t *testing.T) {
	p := pricedPlan()
	p.Pricing.Tiers[0].UnitAmount = types.USD(9_007_199_254_740_993)
	snapshot := spending.PlanSnapshot{Definition: *p, Revision: math.MaxInt64}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	var decoded spending.PlanSnapshot
	if decodeErr := json.Unmarshal(encoded, &decoded); decodeErr != nil {
		t.Fatal(decodeErr)
	}

	if decoded.Revision != math.MaxInt64 ||
		decoded.Definition.Pricing.Tiers[0].UnitAmount.Amount != 9_007_199_254_740_993 {
		t.Fatalf("snapshot JSON lost int64 precision: %s", encoded)
	}

	budget := spending.Budget{
		Name:        "Full range",
		Basis:       "customer_charge",
		Period:      "lifetime",
		LimitMicros: math.MaxInt64,
	}

	encoded, err = json.Marshal(budget)
	if err != nil {
		t.Fatal(err)
	}

	var restored spending.Budget
	if decodeErr := json.Unmarshal(encoded, &restored); decodeErr != nil {
		t.Fatal(decodeErr)
	}

	if restored.LimitMicros != math.MaxInt64 {
		t.Fatalf("budget JSON lost int64 precision: %s", encoded)
	}
}

func TestEngineSharedAcrossConcurrentCallers(t *testing.T) {
	engine := spending.New()
	p := pricedPlan()
	p.Pricing.Tiers[0].UnitQuantity = 1

	for index := range 16 {
		t.Run("settlement", func(t *testing.T) {
			t.Parallel()

			delta := int64(index + 1)

			amount, err := engine.Rate(
				p,
				map[string]spending.MeterTotal{"input_tokens": {Quantity: 10_000}},
				map[string]*int64{"input_tokens": new(delta)},
			)
			if err != nil || amount == nil || *amount != delta {
				t.Fatalf("concurrent Rate = %v, %v; want %d", amount, err, delta)
			}
		})
	}
}
