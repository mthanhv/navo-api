package progress

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func codes(flags []Flag) []string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = f.Code
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var today = day("2026-11-07")

// AC-01: contract 100, ordered 100, received 70, delivered 50 → remaining 50, no plan.
func TestAC01Quantities(t *testing.T) {
	f := Facts{Contract: d("100"), Ordered: d("100"), Received: d("70"), Delivered: d("50")}
	if got := Remaining(f); !got.Equal(d("50")) {
		t.Fatalf("remaining = %s, want 50", got)
	}
	if got := Available(f); !got.Equal(d("20")) {
		t.Fatalf("available = %s, want 20", got)
	}
	if got := codes(Flags(f, today, 14)); !equal(got, []string{"no_plan"}) {
		t.Fatalf("flags = %v, want [no_plan]", got)
	}
}

func TestFlags(t *testing.T) {
	due := day("2026-10-30")
	future := day("2026-12-15")
	cases := []struct {
		name string
		f    Facts
		want []string
	}{
		{"overdue by contract due date sorts before warnings",
			Facts{Contract: d("30"), Ordered: d("30"), Received: d("30"), Delivered: d("18"), DueOn: &due},
			[]string{"delivery_overdue", "no_plan"}},
		{"over delivered is critical and not completed",
			Facts{Contract: d("1000"), Ordered: d("1050"), Received: d("1050"), Delivered: d("1050")},
			[]string{"over_delivered", "over_ordered"}},
		{"stock short for the next firm plan inside the horizon",
			Facts{Contract: d("120"), Ordered: d("120"), Received: d("24"), Delivered: d("0"), DueOn: &future,
				NextPlan: &Plan{On: day("2026-11-12"), Firm: true, Quantity: d("40")}},
			[]string{"stock_short"}},
		{"plan beyond the horizon is not stock short",
			Facts{Contract: d("120"), Ordered: d("120"), Received: d("24"), Delivered: d("0"),
				NextPlan: &Plan{On: day("2026-12-30"), Quantity: d("40")}},
			nil},
		{"past firm plan counts as overdue even before the contract date",
			Facts{Contract: d("10"), Ordered: d("10"), Received: d("10"), Delivered: d("0"), DueOn: &future,
				NextPlan: &Plan{On: day("2026-11-01"), Firm: true, Quantity: d("10")}},
			[]string{"delivery_overdue"}},
		{"past tentative plan is not overdue",
			Facts{Contract: d("10"), Ordered: d("10"), Received: d("10"), Delivered: d("0"),
				NextPlan: &Plan{On: day("2026-11-01"), Quantity: d("10")}},
			nil},
		{"under ordered and receipt overdue",
			Facts{Contract: d("4"), Ordered: d("2"), Received: d("0"), Delivered: d("0"), ReceiptOverdue: true,
				NextPlan: &Plan{On: day("2027-01-10"), Quantity: d("4")}},
			[]string{"receipt_overdue", "under_ordered"}},
		{"fully delivered is completed",
			Facts{Contract: d("24"), Ordered: d("24"), Received: d("24"), Delivered: d("24")},
			[]string{"completed"}},
		{"decimal quantities compare exactly",
			Facts{Contract: d("0.3"), Ordered: d("0.1").Add(d("0.2")), Received: d("0.3"), Delivered: d("0.1").Add(d("0.2"))},
			[]string{"completed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codes(Flags(c.f, today, 14)); !equal(got, c.want) {
				t.Fatalf("flags = %v, want %v", got, c.want)
			}
		})
	}
}

func TestOverdueParams(t *testing.T) {
	due := day("2026-10-30")
	flags := Flags(Facts{Contract: d("30"), Ordered: d("30"), Received: d("30"), Delivered: d("18"), DueOn: &due,
		NextPlan: &Plan{On: day("2026-11-09"), Firm: true, Quantity: d("12")}}, today, 14)
	if flags[0].Code != "delivery_overdue" || flags[0].Params["days_late"] != "8" || flags[0].Params["remaining"] != "12" {
		t.Fatalf("unexpected first flag %+v", flags[0])
	}
}

func TestFlagsNeverNil(t *testing.T) {
	// A healthy line has no flags; it must still encode as [] (the web app relied on it).
	f := Flags(Facts{Contract: d("10"), Ordered: d("10"), Received: d("10"), Delivered: d("0"), NextPlan: &Plan{On: day("2027-01-10"), Quantity: d("10")}}, today, 14)
	if f == nil || len(f) != 0 {
		t.Fatalf("flags = %#v, want empty non-nil slice", f)
	}
}
