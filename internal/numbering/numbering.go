// Package numbering issues document numbers (REQUIREMENTS 5.12): SC-26-0142 for
// contracts, PO-26-0142-01 / GR-… / DO-… for documents of a contract. Issued
// numbers are never reused.
package numbering

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ContractNumber formats SC-{yy}-{nnnn}.
func ContractNumber(year, seq int) string {
	return fmt.Sprintf("SC-%02d-%04d", year%100, seq)
}

// DraftNumber formats the temporary number of an unsigned contract.
func DraftNumber(seq int) string { return fmt.Sprintf("DRAFT-%04d", seq) }

// ChildNumber formats {TYPE}-{yy}-{nnnn}-{pp} from the contract number.
func ChildNumber(docType, contractNumber string, seq int) string {
	return fmt.Sprintf("%s-%s-%02d", docType, strings.TrimPrefix(contractNumber, "SC-"), seq)
}

// NextYearly takes the next value of a yearly series (SC, PI, RV, PV) or of
// the draft series (year 0). The upsert row-locks the counter, so concurrent
// callers get distinct values.
func NextYearly(ctx context.Context, tx pgx.Tx, docType string, year int) (int, error) {
	var v int
	err := tx.QueryRow(ctx, `INSERT INTO document_series (doc_type, year, last_value) VALUES ($1, $2, 1)
		ON CONFLICT (doc_type, year) DO UPDATE SET last_value = document_series.last_value + 1
		RETURNING last_value`, docType, year).Scan(&v)
	return v, err
}

// childCounters maps a document type to its counter column on contracts.
var childCounters = map[string]string{"PO": "po_seq", "GR": "gr_seq", "DO": "do_seq", "SI": "si_seq"}

// YearlyNumber formats {TYPE}-{yy}-{nnnn} for documents that span contracts (PI, RV, PV).
func YearlyNumber(docType string, year, seq int) string {
	return fmt.Sprintf("%s-%02d-%04d", docType, year%100, seq)
}

// NextChild takes the next sequence of a document type within one contract
// and returns the formatted number. The contract row stays locked until commit.
func NextChild(ctx context.Context, tx pgx.Tx, contractID uuid.UUID, docType string) (string, error) {
	col, ok := childCounters[docType]
	if !ok {
		return "", fmt.Errorf("numbering: unknown child type %q", docType)
	}
	var number *string
	var seq int
	err := tx.QueryRow(ctx, `UPDATE contracts SET `+col+` = `+col+` + 1 WHERE id = $1 RETURNING number, `+col, contractID).Scan(&number, &seq)
	if err != nil {
		return "", err
	}
	if number == nil {
		return "", fmt.Errorf("numbering: contract %s has no number yet", contractID)
	}
	return ChildNumber(docType, *number, seq), nil
}
