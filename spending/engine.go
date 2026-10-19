// Package spending provides stateless USD-micros retail pricing and settled
// budget policy over Ledger's native plan definitions. It owns no persistence,
// admission reservations, payment collection, or buffered metering.
package spending

import (
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/ledger"
	"github.com/xraph/ledger/invoice"
	"github.com/xraph/ledger/meter"
	"github.com/xraph/ledger/plan"
)

// MeterTotal keeps known quantity separate from the number of unknown samples.
// Unknown is never a substitute for quantity zero.
type MeterTotal struct {
	Quantity int64 `json:"quantity"`
	Unknown  int64 `json:"unknown"`
}

// Window is a UTC half-open interval [Start, End). Lifetime has no end.
type Window struct {
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"`
}

type Budget struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Basis       string `json:"basis"`
	Period      string `json:"period"`
	LimitMicros int64  `json:"limit_micros"`
	Enabled     bool   `json:"enabled"`
	Revision    int64  `json:"revision"`
}

type PlanSnapshot struct {
	Definition plan.Plan `json:"definition"`
	Revision   int64     `json:"revision"`
}

// Meter describes one measured input, never the computed customer charge.
type Meter struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Unit string `json:"unit"`
}

// BuiltinMeters returns caller-owned catalog storage, so a consumer cannot
// mutate another tenant's or another caller's catalog.
func BuiltinMeters() []Meter {
	return []Meter{
		{Key: "requests", Name: "Dispatched inference requests", Unit: "requests"},
		{Key: "input_tokens", Name: "Input tokens", Unit: "tokens"},
		{Key: "output_tokens", Name: "Output tokens", Unit: "tokens"},
		{Key: "total_tokens", Name: "Total tokens", Unit: "tokens"},
		{Key: "cache_read_tokens", Name: "Cache read tokens", Unit: "tokens"},
		{Key: "cache_write_tokens", Name: "Cache write tokens", Unit: "tokens"},
		{Key: "reasoning_tokens", Name: "Reasoning tokens", Unit: "tokens"},
		{Key: "provider_cost_micros", Name: "Provider cost", Unit: "usd_micros"},
	}
}

func builtin(key string) bool {
	switch key {
	case "requests",
		"input_tokens",
		"output_tokens",
		"total_tokens",
		"cache_read_tokens",
		"cache_write_tokens",
		"reasoning_tokens",
		"provider_cost_micros":
		return true
	default:
		return false
	}
}

// Engine has no mutable state and is safe to share across tenants and goroutines.
type Engine struct{}

func New() *Engine { return &Engine{} }

// ValidatePlan reuses the native write-time validation and restricts spending to
// USD and observable, builtin metered prices. Plugin pricing/aggregation is not
// supported by this stateless engine. Spending is debit-only: flat fees and
// volume per-unit ratios must not decrease at tier boundaries. The native
// invoice package continues to support volume discounts outside spending.
func (*Engine) ValidatePlan(p *plan.Plan) error {
	if validationErr := ledger.ValidatePlan(p); validationErr != nil {
		return validationErr
	}

	if !strings.EqualFold(p.Currency, "usd") {
		return fmt.Errorf("%w: spending plans must use USD micros", ledger.ErrInvalidPricing)
	}

	if p.Metadata["pricing_strategy"] != "" {
		return fmt.Errorf(
			"%w: spending does not execute plugin pricing strategies",
			ledger.ErrInvalidPricing,
		)
	}

	for _, feature := range p.Features {
		if feature.Type == plan.FeatureMetered {
			switch feature.Period {
			case plan.PeriodMonthly, plan.PeriodYearly, plan.PeriodNone:
			default:
				return fmt.Errorf(
					"%w: spending meter %q requires an explicit supported period",
					ledger.ErrInvalidPricing,
					feature.Key,
				)
			}
		}

		if feature.Metadata["pricing_strategy"] != "" || feature.Metadata["aggregator"] != "" {
			return fmt.Errorf(
				"%w: spending feature %q requires native pricing and measured totals",
				ledger.ErrInvalidPricing,
				feature.Key,
			)
		}
	}

	if p.Pricing == nil {
		return nil
	}

	switch p.Pricing.BillingPeriod {
	case plan.PeriodMonthly, plan.PeriodYearly, plan.PeriodNone:
	default:
		return fmt.Errorf(
			"%w: spending requires monthly, yearly, or none billing period",
			ledger.ErrInvalidPricing,
		)
	}

	for _, feature := range p.Features {
		tiers := featureTiers(p, feature.Key)
		if len(tiers) == 0 {
			continue
		}

		if feature.Type != plan.FeatureMetered || !builtin(feature.Key) {
			return fmt.Errorf(
				"%w: spending can price only builtin metered features, not %q",
				ledger.ErrInvalidPricing,
				feature.Key,
			)
		}

		if tiers[0].Type == plan.TierGraduated || len(tiers) == 1 {
			continue // Nonnegative native rates already prove monotonicity.
		}

		sorted := invoice.SortTiers(tiers)

		var previous, current big.Rat

		for index, tier := range sorted {
			switch tier.Type {
			case plan.TierFlat:
				current.SetInt64(tier.FlatAmount.Amount)
			case plan.TierVolume:
				denominator := tier.UnitQuantity
				if denominator == 0 {
					denominator = 1
				}

				current.SetFrac64(tier.UnitAmount.Amount, denominator)
			}

			if index > 0 && current.Cmp(&previous) < 0 {
				return fmt.Errorf(
					"%w: spending feature %q has decreasing %s prices",
					ledger.ErrInvalidPricing,
					feature.Key,
					tier.Type,
				)
			}

			previous.Set(&current)
		}
	}

	return nil
}

func featureTiers(p *plan.Plan, key string) []plan.PriceTier {
	if p.Pricing == nil {
		return nil
	}

	source := p.Pricing.Tiers
	count := 0

	for _, tier := range source {
		if tier.FeatureKey == key {
			count++
		}
	}

	if count == len(source) {
		return source // Read-only native input; no copy for single-meter plans.
	}

	if count == 0 {
		return nil
	}

	tiers := make([]plan.PriceTier, 0, count)

	for _, tier := range source {
		if tier.FeatureKey == key {
			tiers = append(tiers, tier)
		}
	}

	return tiers
}

func (*Engine) ValidateBudget(budget Budget) error {
	if strings.TrimSpace(budget.Name) == "" || !utf8.ValidString(budget.Name) ||
		utf8.RuneCountInString(budget.Name) > 255 {
		return fmt.Errorf(
			"%w: budget name must contain 1 to 255 characters",
			ledger.ErrInvalidInput,
		)
	}

	if budget.Basis != "provider_cost" && budget.Basis != "customer_charge" {
		return fmt.Errorf(
			"%w: budget basis must be provider_cost or customer_charge",
			ledger.ErrInvalidInput,
		)
	}

	switch budget.Period {
	case "day", "month", "lifetime":
	default:
		return fmt.Errorf(
			"%w: budget period must be day, month, or lifetime",
			ledger.ErrInvalidInput,
		)
	}

	if budget.LimitMicros < 0 || budget.Revision < 0 {
		return fmt.Errorf(
			"%w: budget limit and revision must be nonnegative",
			ledger.ErrInvalidInput,
		)
	}

	return nil
}

func (*Engine) Window(period string, at time.Time) (Window, error) {
	at = at.UTC()

	var start, end time.Time

	switch period {
	case "day":
		start = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
	case "month":
		start = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	case "lifetime":
		return Window{Start: time.Unix(0, 0).UTC()}, nil
	default:
		return Window{}, fmt.Errorf("%w: unknown budget period %q", ledger.ErrInvalidInput, period)
	}

	return Window{Start: start, End: &end}, nil
}

// Exceeded checks settled amounts only; zero is a finite enabled limit. It does
// not reserve capacity or prevent concurrent/in-flight settlement overshoot.
func (engine *Engine) Exceeded(budget Budget, settledMicros int64) (bool, error) {
	if validationErr := engine.ValidateBudget(budget); validationErr != nil {
		return false, validationErr
	}

	if settledMicros < 0 {
		return false, fmt.Errorf("%w: settled micros cannot be negative", ledger.ErrInvalidInput)
	}

	return budget.Enabled && settledMicros >= budget.LimitMicros, nil
}

// Rate returns incremental retail overage in USD micros, not a base fee or
// provider cost. Missing prior totals, prior unknown samples, and missing/nil
// deltas for relevant priced meters return nil (unknown), never a fake zero.
// Unlimited features and plans with no tiers are known free. Input maps and the
// plan are not mutated. Cumulative totals are rounded together before their
// difference so splitting fractional usage into requests cannot discard it.
func (engine *Engine) Rate(
	p *plan.Plan,
	before map[string]MeterTotal,
	delta map[string]*int64,
) (*int64, error) {
	if validationErr := engine.ValidatePlan(p); validationErr != nil {
		return nil, validationErr
	}

	for key, total := range before {
		if total.Quantity < 0 || total.Unknown < 0 {
			return nil, fmt.Errorf(
				"%w: negative prior total for %q",
				ledger.ErrInvalidQuantity,
				key,
			)
		}
	}

	for key, quantity := range delta {
		if quantity != nil && *quantity < 0 {
			return nil, fmt.Errorf("%w: negative delta for %q", ledger.ErrInvalidQuantity, key)
		}
	}
	// At most eight distinct builtin meters can be priced. Keep bounded
	// per-call change storage local; the native computation does not retain it.
	var storage [8]invoice.UsageChange

	changes := storage[:0]
	unknown := false

	for _, feature := range p.Features {
		if feature.Limit == -1 {
			continue
		}

		tiers := featureTiers(p, feature.Key)
		if len(tiers) == 0 {
			continue
		}

		prior, exists := before[feature.Key]

		quantity := delta[feature.Key]
		if !exists || quantity == nil {
			unknown = true
			continue
		}

		after, sumErr := meter.CheckedAdd(prior.Quantity, *quantity)
		if sumErr != nil {
			return nil, sumErr
		}

		if prior.Unknown != 0 {
			unknown = true
			continue
		}

		changes = append(
			changes,
			invoice.UsageChange{
				Tiers:    tiers,
				Before:   prior.Quantity,
				After:    after,
				Included: feature.Limit,
			},
		)
	}

	if unknown {
		return nil, nil
	}

	charge, pricingErr := invoice.ComputeIncrementalOverage(changes, "usd")
	if pricingErr != nil {
		return nil, pricingErr
	}

	if charge.Amount < 0 {
		return nil, fmt.Errorf(
			"%w: spending cannot produce a negative debit",
			ledger.ErrInvalidPricing,
		)
	}

	return &charge.Amount, nil
}
