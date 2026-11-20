// Package decimalx parses quantities and money sent as decimal strings
// (BR-12): never floats, never silently coerced.
package decimalx

import (
	"regexp"

	"github.com/shopspring/decimal"

	"navo-api/internal/apperrors"
)

var plain = regexp.MustCompile(`^[0-9]{1,18}(\.[0-9]{1,6})?$`)

// Positive parses a value that must be > 0 (quantities of events).
func Positive(errs *apperrors.FieldErrors, field, raw string) decimal.Decimal {
	return parse(errs, field, raw, true)
}

// NonNegative parses a value that must be >= 0 (prices).
func NonNegative(errs *apperrors.FieldErrors, field, raw string) decimal.Decimal {
	return parse(errs, field, raw, false)
}

func parse(errs *apperrors.FieldErrors, field, raw string, strictlyPositive bool) decimal.Decimal {
	if raw == "" {
		errs.Add(field, "required", nil)
		return decimal.Zero
	}
	if !plain.MatchString(raw) {
		if len(raw) > 1 && raw[0] == '-' && plain.MatchString(raw[1:]) {
			// A negative number is well formed but out of range: say so, not "wrong format".
			if strictlyPositive {
				errs.Add(field, "greater_than", map[string]any{"min": "0"})
			} else {
				errs.Add(field, "not_negative", nil)
			}
			return decimal.Zero
		}
		errs.Add(field, "decimal", map[string]any{"max_scale": 6})
		return decimal.Zero
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		errs.Add(field, "decimal", map[string]any{"max_scale": 6})
		return decimal.Zero
	}
	if strictlyPositive && !d.IsPositive() {
		errs.Add(field, "greater_than", map[string]any{"min": "0"})
	}
	return d
}

// MustParse reads a numeric column selected as ::text; the database is trusted.
func MustParse(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic("decimalx: database returned non-decimal " + s)
	}
	return d
}

// MoneyScale is the number of decimals money is kept to in a currency: whole đồng for VND, cents otherwise.
func MoneyScale(currency string) int32 {
	if currency == "VND" {
		return 0
	}
	return 2
}

// RoundMoney rounds an amount to the currency's smallest unit.
func RoundMoney(d decimal.Decimal, currency string) decimal.Decimal {
	return d.Round(MoneyScale(currency))
}

// MoneyUnit is the currency's smallest unit (1 for VND, 0.01 otherwise), the rounding tolerance.
func MoneyUnit(currency string) decimal.Decimal {
	return decimal.New(1, -MoneyScale(currency))
}

// CheckMoney refuses an amount with more decimals than the currency has (a VND amount is whole
// đồng), instead of silently rounding what the person typed.
func CheckMoney(errs *apperrors.FieldErrors, field string, d decimal.Decimal, currency string) {
	if !d.Equal(RoundMoney(d, currency)) {
		errs.Add(field, MoneyScaleCode(currency), map[string]any{"max_scale": MoneyScale(currency), "currency": currency})
	}
}

// MoneyScaleCode names the error for too many decimals: "whole_amount" when the currency has none.
func MoneyScaleCode(currency string) string {
	if MoneyScale(currency) == 0 {
		return "whole_amount"
	}
	return "decimal"
}
