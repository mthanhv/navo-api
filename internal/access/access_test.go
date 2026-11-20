package access

import (
	"testing"

	"github.com/google/uuid"
)

// Delivery receives and delivers on every contract, without contract edits, ordering, prices or finance (2026-11-20).
func TestDeliveryRole(t *testing.T) {
	p := NewPrincipal(uuid.New(), "Tuấn", "tuan@test.vn", []Role{Delivery}, nil)
	for _, perm := range []Permission{ContractsRead, FulfillmentWrite, FulfillmentConfirm} {
		if !p.Can(perm) {
			t.Errorf("delivery should hold %s", perm)
		}
	}
	for _, perm := range []Permission{ContractsWrite, ProcurementWrite, PartnersWrite, ContactsWrite, PurchaseCostRead, FinanceRead, FinanceWrite, QuantityOverride, UsersManage} {
		if p.Can(perm) {
			t.Errorf("delivery must not hold %s", perm)
		}
	}
	if p.OwnerFilter() != nil {
		t.Error("delivery sees every contract")
	}
	if ValidRole("leader") || !ValidRole("delivery") {
		t.Error("leader is gone and delivery is a role")
	}
}
