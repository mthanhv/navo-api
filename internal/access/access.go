// Package access defines roles, permissions and data scope (TECHNICAL_DESIGN 5.2).
// Admin holds every permission and sees the whole company (owner decision, 2026-11-10;
// before that admin was kept away from business and finance data).
package access

import "github.com/google/uuid"

// Role is a job a user can hold; a user may hold several.
type Role string

const (
	Sales      Role = "sales"
	Leader     Role = "leader"
	Accounting Role = "accounting"
	Admin      Role = "admin"
)

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool {
	switch Role(r) {
	case Sales, Leader, Accounting, Admin:
		return true
	}
	return false
}

// Permission is one action the API checks.
type Permission string

const (
	ContractsRead      Permission = "contracts.read"
	ContractsWrite     Permission = "contracts.write"
	ProcurementWrite   Permission = "procurement.write"
	FulfillmentWrite   Permission = "fulfillment.write"
	FulfillmentConfirm Permission = "fulfillment.confirm"
	PartnersWrite      Permission = "partners.write"
	CatalogWrite       Permission = "catalog.write"
	ReportsOperations  Permission = "reports.operations"
	UsersManage        Permission = "users.manage"
	// Sensitive grants: never implied by a role title, granted one by one.
	PurchaseCostRead Permission = "purchase_cost.read"
	FinanceRead      Permission = "finance.read"
	// FinanceWrite records invoices, receipts, payments and MISA exports (accounting).
	FinanceWrite     Permission = "finance.write"
	QuantityOverride Permission = "quantity.override"
)

// Grantable lists permissions that may be stored in user_grants.
var Grantable = map[Permission]bool{PurchaseCostRead: true, FinanceRead: true, QuantityOverride: true, FulfillmentConfirm: true}

var roleDefaults = map[Role][]Permission{
	Sales:      {ContractsRead, ContractsWrite, ProcurementWrite, FulfillmentWrite, FulfillmentConfirm, PartnersWrite, CatalogWrite, ReportsOperations},
	Leader:     {ContractsRead, ReportsOperations},
	Accounting: {ContractsRead, PurchaseCostRead, FinanceRead, FinanceWrite},
	Admin:      all,
}

// all is every permission; the admin role holds it.
var all = []Permission{ContractsRead, ContractsWrite, ProcurementWrite, FulfillmentWrite, FulfillmentConfirm, PartnersWrite, CatalogWrite,
	ReportsOperations, UsersManage, PurchaseCostRead, FinanceRead, FinanceWrite, QuantityOverride}

// Principal is the signed-in user with resolved permissions.
type Principal struct {
	UserID  uuid.UUID `json:"id"`
	Name    string    `json:"full_name"`
	Email   string    `json:"email"`
	Roles   []Role    `json:"roles"`
	Granted []string  `json:"grants"`
	perms   map[Permission]bool
	seesAll bool
}

// NewPrincipal resolves permissions from roles and explicit grants.
func NewPrincipal(id uuid.UUID, name, email string, roles []Role, grants []string) *Principal {
	p := &Principal{UserID: id, Name: name, Email: email, Roles: roles, Granted: grants, perms: map[Permission]bool{}}
	for _, r := range roles {
		for _, perm := range roleDefaults[r] {
			p.perms[perm] = true
		}
		// Leaders, accounting and admin read the whole company; sales read their own (REQUIREMENTS 2).
		if r == Leader || r == Accounting || r == Admin {
			p.seesAll = true
		}
	}
	for _, g := range grants {
		p.perms[Permission(g)] = true
	}
	return p
}

// Can reports whether the user holds perm.
func (p *Principal) Can(perm Permission) bool { return p != nil && p.perms[perm] }

// Permissions lists resolved permissions (for clients to hide what the user cannot do).
func (p *Principal) Permissions() []Permission {
	out := make([]Permission, 0, len(p.perms))
	for perm := range p.perms {
		out = append(out, perm)
	}
	return out
}

// OwnerFilter returns the owner id that contract queries must be limited to,
// or nil when the user sees the whole company.
func (p *Principal) OwnerFilter() *uuid.UUID {
	if p.seesAll {
		return nil
	}
	id := p.UserID
	return &id
}
