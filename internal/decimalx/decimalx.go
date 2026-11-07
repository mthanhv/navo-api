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
