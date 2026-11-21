package masterdata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"navo-api/internal/apperrors"
	"navo-api/internal/response"
)

// TaxInfo is what the public business register says about a tax code: enough to fill in a
// customer or supplier (name and registered address); the person still checks and saves.
type TaxInfo struct {
	TaxCode           string `json:"tax_code"`
	Name              string `json:"name"`
	ShortName         string `json:"short_name"`
	InternationalName string `json:"international_name"`
	Address           string `json:"address"`
}

var lookupClient = &http.Client{Timeout: 6 * time.Second}

// LookupTaxCode handles GET /organizations/tax-lookup?tax_code=…: the company's registered name and
// address from the configured register (NAVO_TAXCODE_LOOKUP_URL, default VietQR's free business
// lookup). Only the tax code leaves Navo. An empty setting turns the feature off.
func (h *Handlers) LookupTaxCode(c *gin.Context) {
	code := strings.TrimSpace(c.Query("tax_code"))
	if !taxCode.MatchString(code) {
		response.Fail(c, apperrors.Invalid("tax_code", "tax_code", nil))
		return
	}
	if h.TaxLookupURL == "" {
		response.Fail(c, apperrors.Rule("tax_lookup_off", "Looking up tax codes is turned off.", nil))
		return
	}
	info, err := lookupTax(c.Request.Context(), h.TaxLookupURL, code)
	switch {
	case errors.Is(err, errTaxNotFound):
		response.Fail(c, apperrors.Rule("tax_code_not_found", "No company is registered with this tax code.", map[string]any{"tax_code": code}))
	case err != nil:
		response.Fail(c, apperrors.Rule("tax_lookup_unavailable", "The tax code register cannot be reached right now. Fill in the details by hand.", nil))
	default:
		response.OK(c, info)
	}
}

var errTaxNotFound = errors.New("tax code not found")

func lookupTax(ctx context.Context, base, code string) (*TaxInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+url.PathEscape(code), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := lookupClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, errTaxNotFound
	}
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("tax lookup: " + res.Status)
	}
	// VietQR: {"code":"00","desc":"…","data":{"id","name","internationalName","shortName","address"}};
	// any other code means "not found".
	var body struct {
		Code string `json:"code"`
		Data *struct {
			ID                string `json:"id"`
			Name              string `json:"name"`
			InternationalName string `json:"internationalName"`
			ShortName         string `json:"shortName"`
			Address           string `json:"address"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if body.Code != "00" || body.Data == nil || strings.TrimSpace(body.Data.Name) == "" {
		return nil, errTaxNotFound
	}
	return &TaxInfo{TaxCode: code, Name: strings.TrimSpace(body.Data.Name), ShortName: strings.TrimSpace(body.Data.ShortName),
		InternationalName: strings.TrimSpace(body.Data.InternationalName), Address: strings.TrimSpace(body.Data.Address)}, nil
}
