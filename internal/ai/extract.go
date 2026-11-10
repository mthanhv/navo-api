// Package ai reads a customer's contract or purchase order (PDF, photo or Excel) and
// returns a draft for a person to check (REQUIREMENTS 5.10: confirm, don't type).
// Nothing is saved here; the web form shows the draft and the person saves it.
package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/xuri/excelize/v2"

	"navo-api/internal/apperrors"
)

// Extractor calls Claude. Without an API key it is disabled and says so (O-17).
type Extractor struct {
	client  anthropic.Client
	model   string
	enabled bool
}

// New builds an extractor; apiKey "" disables AI drafts.
func New(apiKey, model string) *Extractor {
	if apiKey == "" {
		return &Extractor{}
	}
	return &Extractor{client: anthropic.NewClient(option.WithAPIKey(apiKey), option.WithMaxRetries(2)), model: model, enabled: true}
}

// Enabled reports whether AI drafts are configured.
func (e *Extractor) Enabled() bool { return e != nil && e.enabled }

// Draft is what Claude read from the file, in the customer's own words.
type Draft struct {
	Customer struct {
		Name    string  `json:"name"`
		TaxCode *string `json:"tax_code"`
	} `json:"customer"`
	CustomerReference *string     `json:"customer_reference"`
	Title             string      `json:"title"`
	Currency          string      `json:"currency"`
	SignedOn          *string     `json:"signed_on"`
	DeliveryLocation  *string     `json:"delivery_location"`
	Lines             []DraftLine `json:"lines"`
	TotalOnFile       *string     `json:"total_on_file"`
	PaymentTerms      []DraftTerm `json:"payment_terms"`
	Warnings          []string    `json:"warnings"`
}

// DraftLine is one line as printed. Uncertain marks a value the model could not read clearly.
type DraftLine struct {
	Name      string  `json:"name"`
	Unit      string  `json:"unit"`
	Quantity  string  `json:"quantity"`
	UnitPrice string  `json:"unit_price"`
	DueOn     *string `json:"due_on"`
	Uncertain bool    `json:"uncertain"`
	Note      *string `json:"note"`
}

// DraftTerm is one payment term.
type DraftTerm struct {
	Description string  `json:"description"`
	Percent     *string `json:"percent"`
	Trigger     string  `json:"trigger"`
	DaysAfter   int     `json:"days_after"`
}

var nullableString = map[string]any{"type": []string{"string", "null"}}

// schema is the structured-output schema the answer must follow.
var schema = map[string]any{
	"type": "object", "additionalProperties": false,
	"required": []string{"customer", "customer_reference", "title", "currency", "signed_on", "delivery_location", "lines", "total_on_file", "payment_terms", "warnings"},
	"properties": map[string]any{
		"customer": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "tax_code"},
			"properties": map[string]any{"name": map[string]any{"type": "string"}, "tax_code": nullableString}},
		"customer_reference": nullableString,
		"title":              map[string]any{"type": "string"},
		"currency":           map[string]any{"type": "string"},
		"signed_on":          nullableString,
		"delivery_location":  nullableString,
		"lines": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"name", "unit", "quantity", "unit_price", "due_on", "uncertain", "note"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string"}, "unit": map[string]any{"type": "string"},
				"quantity": map[string]any{"type": "string"}, "unit_price": map[string]any{"type": "string"},
				"due_on": nullableString, "uncertain": map[string]any{"type": "boolean"}, "note": nullableString,
			}}},
		"total_on_file": nullableString,
		"payment_terms": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"description", "percent", "trigger", "days_after"},
			"properties": map[string]any{
				"description": map[string]any{"type": "string"}, "percent": nullableString,
				"trigger":    map[string]any{"type": "string", "enum": []string{"signing", "delivery_complete", "acceptance", "date", "manual"}},
				"days_after": map[string]any{"type": "integer"},
			}}},
		"warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

const system = `You read sales documents (contracts, purchase orders, requests for quotation) that a Vietnamese industrial-equipment distributor receives from its customers, and return what they say as data for a person to check.

Rules:
- Copy names exactly as printed (customer name, item names, units, delivery place). Do not translate, shorten or correct them.
- Numbers: plain decimals with a dot and no thousands separators ("18500000", "1250.5"). Vietnamese documents write 18.500.000 for eighteen million five hundred thousand.
- Dates: YYYY-MM-DD. Currency: the ISO code (VND, USD, EUR); VND when the document shows đồng.
- One line per item row. If a quantity, price or date is hard to read or ambiguous, still give your best reading and set uncertain=true with a short note saying why.
- Payment terms: map each instalment to a trigger: signing (after signing/advance), delivery_complete (after full delivery), acceptance (after acceptance/handover minutes), date (a fixed date) or manual (anything else); days_after is the number of days after that event (0 if none).
- title: a short name for the contract in the document's language, e.g. "Thiết bị đo và van bảo dưỡng 2027".
- Use null for anything the document does not state. Add warnings for things a person should know (missing pages, totals that do not add up, unclear tax treatment).`

// MaxBytes limits uploads sent for reading.
const MaxBytes = 20 << 20

// Extract reads one file. mediaType is application/pdf, an image type, or an Excel type.
func (e *Extractor) Extract(ctx context.Context, mediaType string, data []byte) (*Draft, error) {
	if !e.Enabled() {
		return nil, apperrors.Rule("ai_disabled", "AI drafts are not turned on for this installation.", nil)
	}
	var file anthropic.BetaContentBlockParamUnion
	switch {
	case mediaType == "application/pdf":
		file = anthropic.NewBetaDocumentBlock(anthropic.BetaBase64PDFSourceParam{Data: base64.StdEncoding.EncodeToString(data)})
	case mediaType == "image/jpeg" || mediaType == "image/png" || mediaType == "image/webp" || mediaType == "image/gif":
		file = anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{Data: base64.StdEncoding.EncodeToString(data), MediaType: anthropic.BetaBase64ImageSourceMediaType(mediaType)})
	case strings.Contains(mediaType, "spreadsheet") || mediaType == "application/zip":
		text, err := sheetText(data)
		if err != nil {
			return nil, apperrors.Invalid("file", "file_type", map[string]any{"type": mediaType})
		}
		file = anthropic.NewBetaTextBlock("Spreadsheet contents (sheet by sheet, cells separated by |):\n\n" + text)
	default:
		return nil, apperrors.Invalid("file", "file_type", map[string]any{"type": mediaType})
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	msg, err := e.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     e.model,
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(file, anthropic.NewBetaTextBlock("Read this document and return the contract draft."))},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffortMedium,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: schema},
		},
		// A request declined by a safety classifier is re-run on Anthropic's recommended fallback model.
		Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: "default"},
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			switch {
			case apiErr.StatusCode == 429:
				return nil, apperrors.Rule("ai_busy", "The reading service is busy. Try again in a minute.", nil)
			case apiErr.StatusCode == 400 || apiErr.StatusCode == 413:
				return nil, apperrors.Rule("ai_file_unreadable", "This file could not be read (too large or not supported).", nil)
			}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, apperrors.Rule("ai_timeout", "Reading took too long. Try a shorter file or fewer pages.", nil)
		}
		return nil, fmt.Errorf("ai extract: %w", err)
	}
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return nil, apperrors.Rule("ai_refused", "This file could not be read automatically. Please enter the contract by hand.", nil)
	case anthropic.BetaStopReasonMaxTokens:
		return nil, apperrors.Rule("ai_too_long", "The document is too long to read at once. Split it and try again.", nil)
	}
	var out bytes.Buffer
	for _, b := range msg.Content {
		if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
			out.WriteString(t.Text)
		}
	}
	var d Draft
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		return nil, fmt.Errorf("ai extract: invalid JSON: %w", err)
	}
	if d.Lines == nil {
		d.Lines = []DraftLine{}
	}
	if d.PaymentTerms == nil {
		d.PaymentTerms = []DraftTerm{}
	}
	if d.Warnings == nil {
		d.Warnings = []string{}
	}
	return &d, nil
}

// sheetText turns a workbook into plain text rows (at most 2000 rows over all sheets).
func sheetText(data []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer f.Close()
	var b strings.Builder
	rows := 0
	for _, name := range f.GetSheetList() {
		rs, err := f.GetRows(name)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "## %s\n", name)
		for _, r := range rs {
			if rows >= 2000 {
				break
			}
			line := strings.TrimSpace(strings.Join(r, " | "))
			if strings.Trim(line, "| ") == "" {
				continue
			}
			b.WriteString(line + "\n")
			rows++
		}
	}
	return b.String(), nil
}
