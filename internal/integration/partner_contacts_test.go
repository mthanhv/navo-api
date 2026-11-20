//go:build integration

package integration

import "testing"

// A customer can be added together with its people; the one marked main is the main contact,
// and a bad contact field is reported as contacts[i].field with nothing saved.
func TestPartnerWithContacts(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	khoa := e.login("khoa@test.vn")
	bad := map[string]any{"code": "PCX", "name": "Bad", "is_customer": true, "contacts": []any{map[string]any{"full_name": "A"}, map[string]any{"full_name": "B", "email": "not-an-email"}}}
	r := e.do(khoa, "POST", "/api/v1/organizations", bad, nil)
	if r.Status != 400 {
		t.Fatalf("bad contact refused: %d %v", r.Status, r.Body)
	}
	if f := r.Body["errors"].([]any)[0].(map[string]any); f["field"] != "contacts[1].email" || f["code"] != "email" {
		t.Fatalf("error names the contact field: %v", r.Body["errors"])
	}
	org := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "PCC", "name": "Sông Trà", "is_customer": true, "contacts": []any{
		map[string]any{"full_name": "Trần Huy", "job_title": "Vật tư", "phone": "0903 111 222"},
		map[string]any{"full_name": "Lê Mai", "email": "Mai@STRA.vn", "is_primary": true},
	}}, nil), "with contacts")
	if org["contacts"].(float64) != 2 || org["primary_contact"] != "Lê Mai" {
		t.Fatalf("two contacts, Mai main: %v", org)
	}
	list := e.do(khoa, "GET", "/api/v1/organizations/"+org["id"].(string)+"/contacts", nil, nil).Body["data"].([]any)
	if len(list) != 2 {
		t.Fatalf("contacts: %v", list)
	}
	if r := e.do(khoa, "PATCH", "/api/v1/organizations/"+org["id"].(string), map[string]any{"code": "PCC", "name": "Sông Trà", "is_customer": true, "contacts": []any{map[string]any{"full_name": "X"}}}, nil); r.Status != 400 {
		t.Fatalf("contacts change through their own endpoints: %d", r.Status)
	}
}
