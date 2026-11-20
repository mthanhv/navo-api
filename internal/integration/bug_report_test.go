//go:build integration

package integration

import (
	"strings"
	"testing"
)

// Regressions for the 2026-11-20 bug report: money that could go wrong, and plans that could
// outgrow the contract.
func TestBugReportMoneyAndPlans(t *testing.T) {
	e := setup(t)
	e.user("bugs@test.vn", []string{"admin"})
	actor := e.login("bugs@test.vn")

	t.Run("terms must fit the contract and add up before activation", func(t *testing.T) {
		cust := e.must(201, e.do(actor, "POST", "/api/v1/organizations", map[string]any{"code": "BTERM", "name": "Lọc hóa dầu Sông Trà", "is_customer": true}, nil), "customer")
		prod := e.must(201, e.do(actor, "POST", "/api/v1/products", map[string]any{"name": "Van bug", "unit": "bộ"}, nil), "product")
		line := map[string]any{"product_id": prod["id"], "quantity": "1", "unit_price": "10000000"}
		over := map[string]any{"customer_id": cust["id"], "title": "Over", "currency": "VND", "lines": []any{line},
			"milestones": []any{map[string]any{"name": "All", "amount": "13000000", "trigger": "signing", "days_after": 0}}}
		if r := e.do(actor, "POST", "/api/v1/contracts", over, key("terms-over")); r.Status != 400 {
			t.Fatalf("terms above the contract were accepted: %d %v", r.Status, r.Body)
		}
		c := e.must(201, e.do(actor, "POST", "/api/v1/contracts", map[string]any{"customer_id": cust["id"], "title": "Short", "currency": "VND", "lines": []any{line},
			"milestones": []any{map[string]any{"name": "Advance", "amount": "3000000", "percent": "30", "trigger": "signing", "days_after": 0}}}, key("terms-short")), "contract")
		cid := c["id"].(string)
		if r := e.do(actor, "POST", "/api/v1/contracts/"+cid+"/activate", map[string]any{"signed_on": "2026-08-01"}, key("act-short")); r.reason() != "terms_mismatch" {
			t.Fatalf("activation with incomplete terms: %d %v", r.Status, r.Body)
		}
		bad := e.do(actor, "POST", "/api/v1/contracts/"+cid+"/milestones", map[string]any{"name": "Rest", "amount": "5000000", "percent": "70", "trigger": "acceptance", "days_after": 0}, nil)
		if bad.Status != 400 || !hasFieldCode(bad, "percent_mismatch") {
			t.Fatalf("amount not matching its percentage was accepted: %v", bad.Body)
		}
		e.must(201, e.do(actor, "POST", "/api/v1/contracts/"+cid+"/milestones", map[string]any{"name": "Rest", "amount": "7000000", "percent": "70", "trigger": "acceptance", "days_after": 0}, nil), "rest")
		e.must(200, e.do(actor, "POST", "/api/v1/contracts/"+cid+"/activate", map[string]any{"signed_on": "2026-08-01"}, key("act-full")), "activate")
		// On the active contract the signing term can still be corrected and deleted.
		terms := e.do(actor, "GET", "/api/v1/contracts/"+cid+"/milestones", nil, nil).Body["data"].([]any)
		first := terms[0].(map[string]any)
		if first["editable"] != true || first["deletable"] != true {
			t.Fatalf("signing term on an active contract is locked: %v", first)
		}
		e.must(200, e.do(actor, "PATCH", "/api/v1/milestones/"+first["id"].(string), map[string]any{"version": first["version"], "name": "Advance", "amount": "2000000", "percent": "20", "trigger": "signing", "days_after": 0}, nil), "correct term")
		e.must(200, e.do(actor, "DELETE", "/api/v1/milestones/"+first["id"].(string), nil, key("del-term")), "delete corrected term")
	})

	t.Run("legal invoice numbers ignore case and spaces", func(t *testing.T) {
		f := e.activeContract(actor, "BINV", "10")
		inv := func(legal, k string) result {
			return e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/sales-invoices", map[string]any{"legal_number": legal, "issued_on": "2026-10-01",
				"currency": "VND", "amount_net": "1", "vat_amount": "0", "description": "Test"}, key(k))
		}
		e.must(201, inv("1C26TND 0000123", "inv-1"), "first invoice")
		if r := inv("1c26tnd0000123", "inv-2"); r.Status != 400 || !hasFieldCode(r, "taken") {
			t.Fatalf("same legal number with other case/spaces was accepted: %d %v", r.Status, r.Body)
		}
	})

	t.Run("plans stay within what is unplanned and drafts can be edited", func(t *testing.T) {
		f := e.activeContract(actor, "BPLAN", "10")
		plan := func(qty string, extra map[string]any) result {
			body := map[string]any{"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": qty}}}
			for k, v := range extra {
				body[k] = v
			}
			return e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/delivery-orders", body, nil)
		}
		d := e.must(201, plan("6", nil), "plan 6")
		l := e.line(actor, f)
		if l["planned"] != "6" || l["unplanned"] != "4" || !hasFlag(l, "no_plan") {
			t.Fatalf("partial plan hid the unplanned rest: %v", l)
		}
		if r := plan("5", nil); r.Status != 400 || !hasFieldCode(r, "exceeds_unplanned") {
			t.Fatalf("plan beyond the unplanned quantity was accepted: %d %v", r.Status, r.Body)
		}
		e.must(201, plan("5", map[string]any{"over_contract": true}), "deliberate over-plan")
		edited := e.must(200, e.do(actor, "PATCH", "/api/v1/delivery-orders/"+d["id"].(string), map[string]any{"version": d["version"], "planned_on": "2026-12-01", "firmness": "firm",
			"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": "4"}}}, nil), "edit draft delivery")
		if edited["planned_on"] != "2026-12-01" {
			t.Fatalf("draft delivery not edited: %v", edited)
		}
	})

	t.Run("a refused goods receipt leaves no draft and takes no number", func(t *testing.T) {
		f := e.activeContract(actor, "BGR", "10")
		p := e.must(201, e.do(actor, "POST", "/api/v1/contracts/"+f.contract+"/purchase-orders", map[string]any{"supplier_id": f.supplier, "currency": "VND",
			"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": "5", "unit_price": "1"}}}, nil), "po")
		po, poLine := p["id"].(string), p["lines"].([]any)[0].(map[string]any)["id"].(string)
		e.must(200, e.do(actor, "POST", "/api/v1/purchase-orders/"+po+"/confirm", nil, key("bgr-po")), "po confirm")
		receive := func(qty, k string) result {
			return e.do(actor, "POST", "/api/v1/purchase-orders/"+po+"/goods-receipts", map[string]any{"received_on": "2026-10-01", "confirm": true,
				"lines": []any{map[string]any{"purchase_order_line_id": poLine, "accepted_quantity": qty}}}, key(k))
		}
		if r := receive("9", "bgr-over"); r.reason() != "receipt_exceeds_ordered" {
			t.Fatalf("over-receipt: %d %v", r.Status, r.Body)
		}
		gr := e.must(201, receive("5", "bgr-ok"), "receipt")
		if num, _ := gr["number"].(string); gr["status"] != "confirmed" || !strings.HasSuffix(num, "-01") {
			t.Fatalf("refused receipt used a number or receipt not confirmed: %v", gr)
		}
	})

	t.Run("search ignores Vietnamese accents", func(t *testing.T) {
		items, _ := e.must(200, e.do(actor, "GET", "/api/v1/organizations?role=customer&q=song%20tra", nil, nil), "search")["items"].([]any)
		if len(items) == 0 {
			t.Fatal(`"song tra" did not find "Lọc hóa dầu Sông Trà"`)
		}
	})
}

func hasFieldCode(r result, code string) bool {
	errs, _ := r.Body["errors"].([]any)
	for _, e := range errs {
		if m, ok := e.(map[string]any); ok && m["code"] == code {
			return true
		}
	}
	return false
}

func hasFlag(line map[string]any, code string) bool {
	flags, _ := line["flags"].([]any)
	for _, f := range flags {
		if m, ok := f.(map[string]any); ok && m["code"] == code {
			return true
		}
	}
	return false
}
