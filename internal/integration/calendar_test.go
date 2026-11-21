//go:build integration

package integration

import (
	"testing"
	"time"
)

// The delivery calendar (owner request 2026-11-21): deliveries and purchase orders over a period,
// one row per contract, late firm plans flagged, sales seeing only their contracts.
func TestDeliveryCalendar(t *testing.T) {
	e := setup(t)
	e.user("cal-admin@test.vn", []string{"admin"})
	e.user("cal-sales@test.vn", []string{"sales"})
	admin, sales := e.login("cal-admin@test.vn"), e.login("cal-sales@test.vn")
	f := e.activeContract(admin, "CAL", "10")
	e.receiveAndDeliver(admin, f, "cal", "10", "0") // a fully received PO
	day := func(offset int) string { return time.Now().AddDate(0, 0, offset).Format(time.DateOnly) }
	plan := func(on, firmness, qty string) {
		e.must(201, e.do(admin, "POST", "/api/v1/contracts/"+f.contract+"/delivery-orders", map[string]any{"planned_on": on, "firmness": firmness,
			"lines": []any{map[string]any{"contract_line_id": f.line, "quantity": qty}}}, nil), "plan "+on)
	}
	plan(day(-3), "firm", "4")      // promised and passed: late
	plan(day(10), "tentative", "6") // a plan

	from, to := day(-40), day(40)
	cal := e.must(200, e.do(admin, "GET", "/api/v1/reports/calendar?from="+from+"&to="+to, nil, nil), "calendar")
	rows := cal["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("one contract row expected: %v", rows)
	}
	row := rows[0].(map[string]any)
	states := map[string]int{}
	for _, it := range row["items"].([]any) {
		m := it.(map[string]any)
		states[m["kind"].(string)+":"+m["state"].(string)]++
	}
	if states["do:late"] != 1 || states["do:plan"] != 1 || states["po:done"] != 1 || row["late"] != float64(1) {
		t.Fatalf("calendar items: %v (row %v)", states, row)
	}
	if late := e.must(200, e.do(admin, "GET", "/api/v1/reports/calendar?from="+from+"&to="+to+"&late=1&show=po", nil, nil), "late POs")["rows"].([]any); len(late) != 0 {
		t.Fatalf("no late purchase order, so the late-only PO view is empty: %v", late)
	}
	if own := e.must(200, e.do(sales, "GET", "/api/v1/reports/calendar?from="+from+"&to="+to, nil, nil), "sales calendar")["rows"].([]any); len(own) != 0 {
		t.Fatalf("sales see only their own contracts: %v", own)
	}
	// Expanded lanes over a year: signing, goods received and (for money people) payment terms.
	e.must(201, e.do(admin, "POST", "/api/v1/contracts/"+f.contract+"/milestones", map[string]any{"name": "All", "amount": "10", "trigger": "signing", "days_after": 0}, nil), "term")
	year := e.must(200, e.do(admin, "GET", "/api/v1/reports/calendar?from=2026-07-01&to="+day(40), nil, nil), "year")["rows"].([]any)[0].(map[string]any)
	kinds := map[string]string{}
	for _, it := range year["items"].([]any) {
		m := it.(map[string]any)
		kinds[m["kind"].(string)] = m["state"].(string)
	}
	if kinds["signed"] != "done" || kinds["gr"] != "done" || kinds["pay"] != "late" || year["late"] != float64(2) {
		t.Fatalf("expanded lanes: %v (late %v)", kinds, year["late"])
	}
	if r := e.do(admin, "GET", "/api/v1/reports/calendar?from=2025-01-01&to=2026-12-31", nil, nil); r.Status != 400 {
		t.Fatalf("more than a year is refused: %d", r.Status)
	}
	if r := e.do(admin, "GET", "/api/v1/reports/calendar?from="+to+"&to="+from, nil, nil); r.Status != 400 {
		t.Fatalf("an end before the start is refused: %d", r.Status)
	}
}
