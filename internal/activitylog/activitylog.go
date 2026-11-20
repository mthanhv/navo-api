// Package activitylog writes the activities Navo records by itself (quotation sent, product
// email sent, contract signed) and moves a lead along its stages (REQUIREMENTS 5.14). It has no
// dependencies on handlers, so core, crm and sales can all call it inside their transactions.
package activitylog

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Auto records a finished activity written by Navo, owned by owner (the person it concerns).
func Auto(ctx context.Context, tx pgx.Tx, kind string, org uuid.UUID, contract, quotation *uuid.UUID, owner, by uuid.UUID, summary string) error {
	_, err := tx.Exec(ctx, `INSERT INTO activities (kind, status, organization_id, contract_id, quotation_id, summary, done_at, auto, owner_id, created_by)
		VALUES ($1, 'done', $2, $3, $4, $5, now(), true, $6, $7)`, kind, org, contract, quotation, summary, owner, by)
	return err
}

// Touched moves a new lead to "contacted" after its first finished activity.
func Touched(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE organizations SET lead_stage = 'contacted', version = version + 1, updated_at = now() WHERE id = $1 AND lead_stage = 'new'`, org)
	return err
}

// Quoted moves a lead that has not got further to "quoted" when a quotation is sent to it.
func Quoted(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE organizations SET lead_stage = 'quoted', version = version + 1, updated_at = now()
		WHERE id = $1 AND lead_stage IN ('new', 'contacted', 'qualified', 'later')`, org)
	return err
}

// BecomeCustomer marks a lead as a customer once a contract is made for it (it stays "quoted"
// until the contract is signed).
func BecomeCustomer(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE organizations SET is_customer = true, version = version + 1, updated_at = now() WHERE id = $1 AND NOT is_customer AND lead_stage IS NOT NULL`, org)
	return err
}

// Won closes a lead as won when its first contract is signed (activated).
func Won(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE organizations SET lead_stage = 'won', won_at = now(), is_customer = true, version = version + 1, updated_at = now()
		WHERE id = $1 AND lead_stage IS NOT NULL AND lead_stage <> 'won'`, org)
	return err
}

// CustomerOrLead is the SQL condition for organizations that can receive a quotation or contract.
const CustomerOrLead = `(o.is_customer OR o.lead_stage IS NOT NULL)`
