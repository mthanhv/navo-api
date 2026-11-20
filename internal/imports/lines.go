package imports

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"

	"navo-api/internal/ai"
	"navo-api/internal/apperrors"
	"navo-api/internal/response"
)

// ImportedLine is one item row read from a customer's or our own spreadsheet, ready to drop
// into the contract or quotation form. Problems name what the user must check before saving.
type ImportedLine struct {
	Line       int            `json:"line"`
	Code       string         `json:"product_code"`
	Name       string         `json:"name"`
	Unit       string         `json:"unit"`
	Quantity   *string        `json:"quantity"`
	UnitPrice  *string        `json:"unit_price"`
	TaxRate    *string        `json:"tax_rate"`
	DueOn      *string        `json:"due_on"`
	LeadTime   string         `json:"lead_time"`
	Problems   []string       `json:"problems"`
	Match      *ai.Candidate  `json:"match"`
	Candidates []ai.Candidate `json:"candidates"`
}

const maxLines = 500

// ReadLines handles POST /line-imports (multipart "file", xlsx or csv): reads item lines and
// matches them to the catalogue. Nothing is saved; the form shows them for checking.
func (h *Handlers) ReadLines(c *gin.Context) {
	raw, _, err := readFileAs(c, true)
	if err != nil {
		response.Fail(c, err)
		return
	}
	rows, err := parse("lines", raw)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if len(rows) == 0 {
		response.Fail(c, apperrors.Rule("import_empty", "The file has no item rows under the header.", nil))
		return
	}
	if len(rows) > maxLines {
		response.Fail(c, apperrors.Invalid("file", "too_many", map[string]any{"max": maxLines}))
		return
	}
	out := make([]ImportedLine, len(rows))
	names, codes := make([]string, len(rows)), make([]string, len(rows))
	for i, r := range rows {
		v := r.Values
		l := ImportedLine{Line: r.Line, Code: v["product_code"], Name: v["name"], Unit: v["unit"], LeadTime: v["lead_time"], Problems: []string{}}
		if l.Name == "" {
			l.Problems = append(l.Problems, "name")
		}
		if q, ok := number(v["quantity"]); ok && q.IsPositive() {
			s := q.String()
			l.Quantity = &s
		} else {
			l.Problems = append(l.Problems, "quantity")
		}
		if v["unit_price"] != "" {
			if p, ok := number(v["unit_price"]); ok && !p.IsNegative() {
				s := p.String()
				l.UnitPrice = &s
			} else {
				l.Problems = append(l.Problems, "unit_price")
			}
		}
		if v["vat"] != "" {
			if rate, ok := vatRate(v["vat"]); ok {
				s := rate.String()
				l.TaxRate = &s
			} else {
				l.Problems = append(l.Problems, "vat")
			}
		}
		if v["due_on"] != "" {
			if d, ok := date(v["due_on"]); ok {
				l.DueOn = &d
			} else {
				l.Problems = append(l.Problems, "due_on")
			}
		}
		if len([]rune(l.Name)) > 500 || len([]rune(l.LeadTime)) > 100 || len([]rune(l.Unit)) > 40 {
			l.Problems = append(l.Problems, "too_long")
		}
		out[i], names[i], codes[i] = l, l.Name, l.Code
	}
	matches, err := ai.MatchProducts(c.Request.Context(), h.Pool, names, codes)
	if err != nil {
		response.Fail(c, err)
		return
	}
	for i := range out {
		out[i].Match, out[i].Candidates = matches[i].Match, matches[i].Candidates
		if out[i].Unit == "" && out[i].Match != nil {
			out[i].Unit = out[i].Match.Unit
		}
	}
	response.OK(c, gin.H{"lines": out})
}

var groupedThousands = regexp.MustCompile(`^\d{1,3}([.,]\d{3})+$`)

// number reads 1234.5, 1,234.5, 1.234,5, 1.000.000 or 12,5 (Vietnamese decimal comma).
func number(s string) (decimal.Decimal, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "đ"), "VND")
	if s == "" {
		return decimal.Zero, false
	}
	dot, comma := strings.LastIndex(s, "."), strings.LastIndex(s, ",")
	switch {
	case dot >= 0 && comma >= 0: // both: the later one is the decimal mark
		if dot > comma {
			s = strings.ReplaceAll(s, ",", "")
		} else {
			s = strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), ",", ".")
		}
	case groupedThousands.MatchString(s) && (strings.Count(s, ".") > 1 || strings.Count(s, ",") > 1 || comma >= 0):
		s = strings.NewReplacer(".", "", ",", "").Replace(s) // 1.000.000, 1,000,000 or 1,000
	case comma >= 0:
		s = strings.ReplaceAll(s, ",", ".")
	}
	d, err := decimal.NewFromString(s)
	return d, err == nil
}

// vatRate accepts 8, 8%, 0.08 or "10 %" and returns the fraction (0.08).
func vatRate(s string) (decimal.Decimal, bool) {
	d, ok := number(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if !ok || d.IsNegative() {
		return decimal.Zero, false
	}
	if d.GreaterThan(decimal.NewFromInt(1)) || strings.Contains(s, "%") {
		d = d.Div(decimal.NewFromInt(100))
	}
	if d.GreaterThan(decimal.NewFromInt(1)) {
		return decimal.Zero, false
	}
	return d, true
}

// date accepts 2026-12-15, 15/12/2026 or an Excel date serial.
func date(s string) (string, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.DateOnly, "02/01/2006", "2/1/2006", "02-01-2006", "02.01.2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(time.DateOnly), true
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 20000 && f < 80000 {
		if t, err := excelize.ExcelDateToTime(f, false); err == nil {
			return t.Format(time.DateOnly), true
		}
	}
	return "", false
}
