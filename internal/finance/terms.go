package finance

import (
	"context"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"navo-api/internal/apperrors"
	"navo-api/internal/db"
	"navo-api/internal/decimalx"
)

// ContractTotalSQL is a contract's value including VAT for the contract aliased c: each line's net
// plus its VAT, VAT rounded to the currency's smallest unit (whole đồng for VND). Lines without a
// VAT rate count without VAT.
const ContractTotalSQL = `(SELECT coalesce(sum(round(cl.quantity * cl.unit_price, 2)
	+ round(cl.quantity * cl.unit_price * coalesce(cl.tax_rate, 0), CASE WHEN c.currency = 'VND' THEN 0 ELSE 2 END)), 0)
	FROM contract_lines cl WHERE cl.contract_id = c.id)`

// ContractTotal returns the contract's value including VAT and its currency.
func ContractTotal(ctx context.Context, q db.Querier, contractID uuid.UUID) (decimal.Decimal, string, error) {
	var total, currency string
	if err := q.QueryRow(ctx, `SELECT `+ContractTotalSQL+`::text, c.currency FROM contracts c WHERE c.id = $1`, contractID).Scan(&total, &currency); err != nil {
		return decimal.Zero, "", err
	}
	return decimalx.MustParse(total), currency, nil
}

func termsSum(ctx context.Context, q db.Querier, contractID uuid.UUID, except *uuid.UUID) (decimal.Decimal, error) {
	var sum string
	err := q.QueryRow(ctx, `SELECT coalesce(sum(amount), 0)::text FROM payment_milestones WHERE contract_id = $1 AND ($2::uuid IS NULL OR id <> $2)`,
		contractID, except).Scan(&sum)
	return decimalx.MustParse(sum), err
}

// checkTerm makes one term agree with its percentage and keeps all terms within the contract
// value. field prefixes the error path ("" for a single term, "milestones[2]." in a draft).
func checkTerm(ctx context.Context, q db.Querier, contractID uuid.UUID, except *uuid.UUID, field string, pct *decimal.Decimal, amount decimal.Decimal) error {
	total, currency, err := ContractTotal(ctx, q, contractID)
	if err != nil {
		return err
	}
	if !amount.Equal(decimalx.RoundMoney(amount, currency)) {
		return apperrors.Invalid(field+"amount", decimalx.MoneyScaleCode(currency), map[string]any{"max_scale": decimalx.MoneyScale(currency), "currency": currency})
	}
	if pct != nil && total.IsPositive() {
		expected := decimalx.RoundMoney(total.Mul(*pct).Div(decimal.NewFromInt(100)), currency)
		if amount.Sub(expected).Abs().GreaterThan(decimalx.MoneyUnit(currency)) {
			return apperrors.Invalid(field+"amount", "percent_mismatch", map[string]any{"expected": expected.String(), "percent": pct.String()})
		}
	}
	others, err := termsSum(ctx, q, contractID, except)
	if err != nil {
		return err
	}
	if others.Add(amount).GreaterThan(total) {
		return apperrors.Invalid(field+"amount", "terms_exceed_value", map[string]any{
			"total": total.String(), "others": others.String(), "max": decimal.Max(total.Sub(others), decimal.Zero).String()})
	}
	return nil
}

// CheckTermsComplete is the activation rule: when a contract has payment terms they must add up
// to its value including VAT, so receivables never show more (or less) than the contract.
func CheckTermsComplete(ctx context.Context, q db.Querier, contractID uuid.UUID) error {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM payment_milestones WHERE contract_id = $1`, contractID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	total, _, err := ContractTotal(ctx, q, contractID)
	if err != nil {
		return err
	}
	sum, err := termsSum(ctx, q, contractID, nil)
	if err != nil {
		return err
	}
	if !sum.Equal(total) {
		return apperrors.Rule("terms_mismatch", "Payment terms must add up to the contract value before activation.",
			map[string]any{"terms": sum.String(), "total": total.String()})
	}
	return nil
}
