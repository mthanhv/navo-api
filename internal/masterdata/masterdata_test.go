package masterdata

import "testing"

func TestOrganizationInvoiceInfoValidation(t *testing.T) {
	in := organizationInput{
		Code: "CUST", Name: "Customer", IsCustomer: true,
		InvoiceCompanyName: stringPtr(" Invoice company "),
		InvoiceTaxCode:     stringPtr("0101234567"),
		InvoiceAddress:     stringPtr(" 1 Main Street "),
		InvoiceEmail:       stringPtr("AP@EXAMPLE.VN"),
	}
	if err := in.validate(); err != nil {
		t.Fatalf("valid invoice details rejected: %v", err)
	}
	if *in.InvoiceCompanyName != "Invoice company" || *in.InvoiceAddress != "1 Main Street" || *in.InvoiceEmail != "ap@example.vn" {
		t.Fatalf("invoice details not normalized: %+v", in)
	}

	for _, tc := range []struct {
		name  string
		input organizationInput
	}{
		{name: "invalid invoice tax code", input: organizationInput{Code: "CUST", Name: "Customer", IsCustomer: true, InvoiceTaxCode: stringPtr("bad")}},
		{name: "invalid invoice email", input: organizationInput{Code: "CUST", Name: "Customer", IsCustomer: true, InvoiceEmail: stringPtr("not-an-email")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.input.validate(); err == nil {
				t.Fatal("invalid invoice details accepted")
			}
		})
	}
}

func stringPtr(s string) *string { return &s }
