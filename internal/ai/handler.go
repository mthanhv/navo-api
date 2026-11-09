package ai

import (
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gabriel-vasile/mimetype"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"navo-api/internal/apperrors"
	"navo-api/internal/response"
)

// Handlers serve AI drafts.
type Handlers struct {
	Pool *pgxpool.Pool
	AI   *Extractor
	TZ   *time.Location
}

// Status handles GET /ai/status so the web shows or hides "Đọc từ file".
func (h *Handlers) Status(c *gin.Context) {
	response.OK(c, gin.H{"enabled": h.AI.Enabled()})
}

// Candidate is a catalogue entry that may be what a line or customer means.
type Candidate struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Code *string   `json:"code"`
	Unit string    `json:"unit,omitempty"`
}

// LineMatch pairs a read line with catalogue products: one sure match, or choices.
type LineMatch struct {
	Match      *Candidate  `json:"match"`
	Candidates []Candidate `json:"candidates"`
}

// ContractDraft handles POST /ai/contract-draft (multipart "file"): reads the file and
// returns the draft plus catalogue matches. Nothing is saved.
func (h *Handlers) ContractDraft(c *gin.Context) {
	ctx := c.Request.Context()
	if !h.AI.Enabled() {
		response.Fail(c, apperrors.Rule("ai_disabled", "AI drafts are not turned on for this installation.", nil))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBytes+1<<20)
	fh, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, apperrors.Invalid("file", "required", nil))
		return
	}
	if fh.Size > MaxBytes {
		response.Fail(c, apperrors.Invalid("file", "file_too_large", map[string]any{"max_mb": MaxBytes >> 20}))
		return
	}
	src, err := fh.Open()
	if err != nil {
		response.Fail(c, err)
		return
	}
	data, err := io.ReadAll(src)
	src.Close()
	if err != nil {
		response.Fail(c, err)
		return
	}
	mt := mimetype.Detect(data).String()
	if i := strings.IndexByte(mt, ';'); i > 0 {
		mt = mt[:i]
	}
	draft, err := h.AI.Extract(ctx, mt, data)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// Customer: by tax code, then by name.
	var customer *Candidate
	customers := []Candidate{}
	if draft.Customer.TaxCode != nil && *draft.Customer.TaxCode != "" {
		var cand Candidate
		if h.Pool.QueryRow(ctx, `SELECT id, name, code FROM organizations WHERE is_customer AND tax_code = $1`, strings.TrimSpace(*draft.Customer.TaxCode)).
			Scan(&cand.ID, &cand.Name, &cand.Code) == nil {
			customer = &cand
		}
	}
	if customer == nil {
		rows, err := h.Pool.Query(ctx, `SELECT id, name, code FROM organizations WHERE is_customer`)
		if err != nil {
			response.Fail(c, err)
			return
		}
		for rows.Next() {
			var cand Candidate
			if err := rows.Scan(&cand.ID, &cand.Name, &cand.Code); err != nil {
				rows.Close()
				response.Fail(c, err)
				return
			}
			customers = append(customers, cand)
		}
		rows.Close()
		ranked := rank(draft.Customer.Name, customers, func(x Candidate) string { return x.Name })
		if len(ranked) > 0 && ranked[0].score >= 0.999 {
			customer = &ranked[0].c
		}
		customers = top(ranked, 3)
	}

	// Products: an exact name is a match; similar names are offered as choices.
	rows, err := h.Pool.Query(ctx, `SELECT id, name, manufacturer_code, unit FROM products`)
	if err != nil {
		response.Fail(c, err)
		return
	}
	var products []Candidate
	for rows.Next() {
		var cand Candidate
		if err := rows.Scan(&cand.ID, &cand.Name, &cand.Code, &cand.Unit); err != nil {
			rows.Close()
			response.Fail(c, err)
			return
		}
		products = append(products, cand)
	}
	rows.Close()
	matches := make([]LineMatch, len(draft.Lines))
	computed := decimal.Zero
	for i, l := range draft.Lines {
		ranked := rank(l.Name, products, func(x Candidate) string { return x.Name })
		matches[i].Candidates = top(ranked, 3)
		if len(ranked) > 0 && ranked[0].score >= 0.999 && (len(ranked) == 1 || ranked[1].score < 0.999) {
			matches[i].Match = &ranked[0].c
			matches[i].Candidates = []Candidate{}
		}
		q, qe := decimal.NewFromString(l.Quantity)
		p, pe := decimal.NewFromString(l.UnitPrice)
		if qe == nil && pe == nil {
			computed = computed.Add(q.Mul(p).Round(2))
		}
	}
	totalMatches := false
	if draft.TotalOnFile != nil {
		if t, err := decimal.NewFromString(*draft.TotalOnFile); err == nil {
			totalMatches = t.Equal(computed)
		}
	}
	response.OK(c, gin.H{
		"draft": draft, "customer_match": customer, "customer_candidates": customers,
		"line_matches": matches, "computed_total": computed.String(), "total_matches": totalMatches,
		"file_name": fh.Filename,
	})
}

type scored struct {
	c     Candidate
	score float64
}

// rank scores candidates by shared words (accent-insensitive); 1 = same name.
func rank(name string, all []Candidate, key func(Candidate) string) []scored {
	want := words(name)
	out := []scored{}
	for _, cand := range all {
		have := words(key(cand))
		if strings.Join(want, " ") == strings.Join(have, " ") && len(want) > 0 {
			out = append(out, scored{cand, 1})
			continue
		}
		common := 0
		set := map[string]bool{}
		for _, w := range have {
			set[w] = true
		}
		for _, w := range want {
			if set[w] {
				common++
			}
		}
		if common == 0 {
			continue
		}
		s := float64(common) / float64(max(len(want), len(have)))
		if s >= 0.4 {
			out = append(out, scored{cand, s * 0.99})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

func top(s []scored, n int) []Candidate {
	out := []Candidate{}
	for i := 0; i < len(s) && i < n; i++ {
		out = append(out, s[i].c)
	}
	return out
}

var fold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

func words(s string) []string {
	s, _, _ = transform.String(fold, strings.ToLower(s))
	s = strings.ReplaceAll(s, "đ", "d")
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
