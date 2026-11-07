// Package progress computes a contract line's quantities and health flags
// (REQUIREMENTS 5.4 and 5.4.1). It is pure so web, mobile, notifications and
// exports share one definition, and it is unit tested.
package progress

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Plan is the nearest open (draft) delivery order for the line.
type Plan struct {
	Number   string          `json:"number"`
	On       time.Time       `json:"planned_on"`
	Firm     bool            `json:"firm"`
	Quantity decimal.Decimal `json:"quantity"`
}

// Facts are the inputs for one contract line. Quantities are already netted:
// Ordered counts confirmed POs, Received accepted goods of confirmed receipts,
// Delivered confirmed delivery orders.
type Facts struct {
	Contract       decimal.Decimal
	Ordered        decimal.Decimal
	Received       decimal.Decimal
	Delivered      decimal.Decimal
	DueOn          *time.Time
	NextPlan       *Plan
	ReceiptOverdue bool
}

// Severity orders flags; the list shows the most severe first.
type Severity string

const (
	Critical Severity = "critical"
	Warning  Severity = "warning"
	Info     Severity = "info"
	Done     Severity = "done"
)

var rank = map[Severity]int{Critical: 0, Warning: 1, Info: 2, Done: 3}

// Flag is one health signal with the values its wording needs.
type Flag struct {
	Code     string            `json:"code"`
	Severity Severity          `json:"severity"`
	Params   map[string]string `json:"params,omitempty"`
}

// Remaining is max(contract - delivered, 0); an excess is reported by Over, never hidden.
func Remaining(f Facts) decimal.Decimal {
	return decimal.Max(f.Contract.Sub(f.Delivered), decimal.Zero)
}

// Over is the quantity delivered beyond the contract, or zero.
func Over(f Facts) decimal.Decimal {
	return decimal.Max(f.Delivered.Sub(f.Contract), decimal.Zero)
}

// Available is what has arrived for the line and not yet gone to the customer.
func Available(f Facts) decimal.Decimal {
	return decimal.Max(f.Received.Sub(f.Delivered), decimal.Zero)
}

// Flags evaluates every rule; today is the business date in the company's
// timezone and horizonDays the "upcoming" window (O-14, default 14).
func Flags(f Facts, today time.Time, horizonDays int) []Flag {
	flags := []Flag{} // never nil: clients always receive a JSON array
	add := func(code string, sev Severity, params map[string]string) {
		flags = append(flags, Flag{Code: code, Severity: sev, Params: params})
	}
	remaining := Remaining(f)
	open := remaining.IsPositive()

	if open {
		duePassed := f.DueOn != nil && f.DueOn.Before(today)
		firmPlanPassed := f.NextPlan != nil && f.NextPlan.Firm && f.NextPlan.On.Before(today)
		if duePassed || firmPlanPassed {
			p := map[string]string{"remaining": remaining.String()}
			if duePassed {
				p["due_on"] = f.DueOn.Format(time.DateOnly)
				p["days_late"] = itoa(int(today.Sub(*f.DueOn).Hours() / 24))
			}
			add("delivery_overdue", Critical, p)
		}
	}
	if over := Over(f); over.IsPositive() {
		add("over_delivered", Critical, map[string]string{"over": over.String()})
	}
	if open && f.NextPlan == nil {
		add("no_plan", Warning, map[string]string{"remaining": remaining.String(), "available": Available(f).String()})
	}
	if f.NextPlan != nil && !f.NextPlan.On.After(today.AddDate(0, 0, horizonDays)) && f.NextPlan.Quantity.GreaterThan(Available(f)) {
		add("stock_short", Warning, map[string]string{
			"planned_on": f.NextPlan.On.Format(time.DateOnly), "needed": f.NextPlan.Quantity.String(), "available": Available(f).String(),
		})
	}
	if f.ReceiptOverdue {
		add("receipt_overdue", Warning, nil)
	}
	if f.Ordered.GreaterThan(f.Contract) {
		add("over_ordered", Warning, map[string]string{"over": f.Ordered.Sub(f.Contract).String()})
	}
	if open && f.Ordered.LessThan(f.Contract) {
		add("under_ordered", Info, map[string]string{"missing": f.Contract.Sub(f.Ordered).String()})
	}
	if !open && !Over(f).IsPositive() {
		add("completed", Done, nil)
	}
	sort.SliceStable(flags, func(i, j int) bool { return rank[flags[i].Severity] < rank[flags[j].Severity] })
	return flags
}

func itoa(n int) string { return decimal.NewFromInt(int64(n)).String() }
