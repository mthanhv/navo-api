package masterdata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLookupTax(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/0310880481"):
			_, _ = w.Write([]byte(`{"code":"00","desc":"ok","data":{"id":"0310880481","name":" CÔNG TY TNHH KỸ THUẬT NHẬT DUY ","internationalName":"NHAT DUY ENGINEERING CO., LTD","shortName":"NHẬT DUY","address":"Quận 12, TP Hồ Chí Minh"}}`))
		case strings.HasSuffix(r.URL.Path, "/0000000000"):
			_, _ = w.Write([]byte(`{"code":"52","desc":"Not found","data":null}`))
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	info, err := lookupTax(ctx, srv.URL+"/", "0310880481")
	if err != nil || info.Name != "CÔNG TY TNHH KỸ THUẬT NHẬT DUY" || info.ShortName != "NHẬT DUY" || info.Address == "" {
		t.Fatalf("found: %+v %v", info, err)
	}
	if _, err := lookupTax(ctx, srv.URL, "0000000000"); !errors.Is(err, errTaxNotFound) {
		t.Fatalf("not registered should be not found: %v", err)
	}
	if _, err := lookupTax(ctx, srv.URL, "1111111111"); err == nil || errors.Is(err, errTaxNotFound) {
		t.Fatalf("register failure must not read as not found: %v", err)
	}
}
