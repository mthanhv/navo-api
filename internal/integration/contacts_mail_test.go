//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"navo-api/internal/mailer"
)

// outbox records emails instead of sending them; addresses at fail.vn bounce.
type outbox struct {
	mu   sync.Mutex
	sent []mailer.Message
	off  bool
}

func (o *outbox) Enabled() bool { return !o.off }
func (o *outbox) From() string  { return "sales@navo.test" }
func (o *outbox) Send(_ context.Context, m mailer.Message) error {
	if strings.HasSuffix(m.ToEmail, "@fail.vn") {
		return errors.New("550 mailbox unavailable")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sent = append(o.sent, m)
	return nil
}

// A partner has several contacts with one main person; product news goes only to
// customer contacts who have an address and still accept it, one copy each, once.
func TestContactsAndProductEmails(t *testing.T) {
	e := setup(t)
	e.user("khoa@test.vn", []string{"sales"})
	e.user("ha@test.vn", []string{"sales"})
	e.user("lan@test.vn", []string{"leader"})
	khoa, ha, lan := e.login("khoa@test.vn"), e.login("ha@test.vn"), e.login("lan@test.vn")
	cust := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "DPN", "name": "Đạm Phương Nam", "is_customer": true}, nil), "customer")["id"].(string)
	supp := e.must(201, e.do(khoa, "POST", "/api/v1/organizations", map[string]any{"code": "DA", "name": "Thiết bị Đông Á", "is_supplier": true}, nil), "supplier")["id"].(string)

	huy := e.must(201, e.do(khoa, "POST", "/api/v1/organizations/"+cust+"/contacts", map[string]any{"full_name": "Trần Huy", "job_title": "Trưởng phòng vật tư", "email": "Huy@DPN.vn", "phone": "0903 111 222"}, nil), "first contact")
	if huy["is_primary"] != true || huy["email"] != "huy@dpn.vn" || huy["accepts_email"] != true {
		t.Fatalf("the first contact is the main one, email lower-cased: %v", huy)
	}
	lanCt := e.must(201, e.do(khoa, "POST", "/api/v1/organizations/"+cust+"/contacts", map[string]any{"full_name": "Lê Lan", "email": "lan@fail.vn", "is_primary": true}, nil), "second contact")
	quiet := e.must(201, e.do(khoa, "POST", "/api/v1/organizations/"+cust+"/contacts", map[string]any{"full_name": "Phạm An", "email": "an@dpn.vn", "accepts_email": false}, nil), "opted out")
	noMail := e.must(201, e.do(khoa, "POST", "/api/v1/organizations/"+cust+"/contacts", map[string]any{"full_name": "Chú Bảy", "phone": "0908"}, nil), "phone only")
	rep := e.must(201, e.do(khoa, "POST", "/api/v1/organizations/"+supp+"/contacts", map[string]any{"full_name": "Mr. Kim", "email": "kim@dongA.vn"}, nil), "supplier contact")
	if r := e.do(khoa, "POST", "/api/v1/organizations/"+cust+"/contacts", map[string]any{"full_name": "Huy 2", "email": "huy@dpn.vn"}, nil); r.Status != 400 {
		t.Fatalf("the same address twice at one partner is refused: %d %v", r.Status, r.Body)
	}
	if r := e.do(khoa, "POST", "/api/v1/organizations/"+cust+"/contacts", map[string]any{"full_name": "X", "email": "not an email"}, nil); r.Status != 400 {
		t.Fatalf("bad address: %d", r.Status)
	}
	if r := e.do(lan, "POST", "/api/v1/organizations/"+cust+"/contacts", map[string]any{"full_name": "X"}, nil); r.Status != 403 {
		t.Fatalf("leaders read contacts but do not edit them: %d", r.Status)
	}
	list := e.do(lan, "GET", "/api/v1/organizations/"+cust+"/contacts", nil, nil).Body["data"].([]any)
	if len(list) != 4 || list[0].(map[string]any)["full_name"] != "Lê Lan" {
		t.Fatalf("four contacts, the new main one first: %v", list)
	}
	orgs := e.must(200, e.do(khoa, "GET", "/api/v1/organizations?role=customer&q=0903", nil, nil), "search by contact phone")["items"].([]any)
	if len(orgs) != 1 || orgs[0].(map[string]any)["contacts"].(float64) != 4 || orgs[0].(map[string]any)["primary_contact"] != "Lê Lan" {
		t.Fatalf("partner list carries the contact summary: %v", orgs)
	}
	e.must(200, e.do(khoa, "DELETE", "/api/v1/contacts/"+noMail["id"].(string), nil, nil), "delete")

	// Product details and the usual supplier.
	prod := e.must(201, e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "Van cầu DN80 PN40", "unit": "bộ", "manufacturer": "KSB", "manufacturer_code": "BOA-H",
		"description": "Thân thép WCB, nối bích", "origin": "Đức", "supplier_id": supp, "supplier_item_code": "GV-80"}, nil), "product")
	if prod["supplier_name"] != "Thiết bị Đông Á" || prod["origin"] != "Đức" {
		t.Fatalf("product keeps its details: %v", prod)
	}
	if r := e.do(khoa, "POST", "/api/v1/products", map[string]any{"name": "X", "unit": "bộ", "supplier_id": cust}, nil); r.Status != 400 {
		t.Fatalf("the usual supplier must be a supplier: %d", r.Status)
	}
	cleared := e.must(200, e.do(khoa, "PATCH", "/api/v1/products/"+prod["id"].(string), map[string]any{"name": "Van cầu DN80 PN40", "unit": "bộ", "manufacturer": "KSB", "manufacturer_code": "BOA-H", "description": "Thân thép WCB, nối bích", "origin": "Đức"}, nil), "clear supplier")
	if cleared["supplier_id"] != nil {
		t.Fatalf("supplier is optional: %v", cleared)
	}

	// Who may be emailed: customer contacts with an address who still accept it.
	able := e.do(khoa, "GET", "/api/v1/contacts/emailable", nil, nil).Body["data"].([]any)
	if len(able) != 2 {
		t.Fatalf("emailable = Huy and Lan only: %v", able)
	}
	mail := map[string]any{"subject": "Giới thiệu van cầu DN80", "body": "Bên em vừa có dòng van mới, gửi anh chị tham khảo.", "product_ids": []string{prod["id"].(string)},
		"contact_ids": []string{huy["id"].(string), lanCt["id"].(string), quiet["id"].(string), rep["id"].(string)}}
	pv := e.must(200, e.do(khoa, "POST", "/api/v1/mailings/preview", mail, nil), "preview")
	text := pv["text"].(string)
	if pv["recipients"].(float64) != 2 || !strings.Contains(text, "Van cầu DN80 PN40 (Hãng: KSB · Mã: BOA-H · Xuất xứ: Đức)") || !strings.Contains(text, "Thân thép WCB") || !strings.Contains(text, "dừng gửi") {
		t.Fatalf("preview shows what will be sent to the two who may receive it: %v", pv)
	}
	if r := e.do(khoa, "POST", "/api/v1/mailings", mail, nil); r.Status != 400 {
		t.Fatalf("sending needs an Idempotency-Key: %d", r.Status)
	}
	if r := e.do(lan, "POST", "/api/v1/mailings", mail, key("m0")); r.Status != 403 {
		t.Fatalf("leaders do not send: %d", r.Status)
	}
	sent := e.must(201, e.do(khoa, "POST", "/api/v1/mailings", mail, key("m1")), "send")
	if r := e.do(khoa, "POST", "/api/v1/mailings", mail, key("m1")); r.Status != 201 || r.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("a retry returns the first result: %d", r.Status)
	}
	id := sent["id"].(string)
	var got map[string]any
	for i := 0; i < 100; i++ {
		if got = e.must(200, e.do(khoa, "GET", "/api/v1/mailings/"+id, nil, nil), "mailing"); got["finished_at"] != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got["total"].(float64) != 2 || got["sent"].(float64) != 1 || got["failed"].(float64) != 1 {
		t.Fatalf("one delivered, one bounced: %v", got)
	}
	e.mail.mu.Lock()
	n, first := len(e.mail.sent), e.mail.sent[0]
	e.mail.mu.Unlock()
	if n != 1 || first.ToEmail != "huy@dpn.vn" || first.ReplyToEmail != "khoa@test.vn" || !strings.HasPrefix(first.Text, "Kính gửi Anh/Chị Trần Huy,") || !strings.Contains(first.Text, "Hãng: KSB") {
		t.Fatalf("one copy, addressed to the person, replies go to the writer: %d %+v", n, first)
	}
	if r := e.do(ha, "GET", "/api/v1/mailings/"+id, nil, nil); r.Status != 404 {
		t.Fatalf("another salesperson does not see it: %d", r.Status)
	}
	if l := e.must(200, e.do(lan, "GET", "/api/v1/mailings", nil, nil), "leader list")["items"].([]any); len(l) != 1 {
		t.Fatalf("leaders see all mailings: %v", l)
	}
	// Deleting a contact keeps the record of what was sent.
	e.must(200, e.do(khoa, "DELETE", "/api/v1/contacts/"+huy["id"].(string), nil, nil), "delete sent contact")
	if rc := e.must(200, e.do(khoa, "GET", "/api/v1/mailings/"+id, nil, nil), "after delete")["recipients"].([]any); len(rc) != 2 {
		t.Fatalf("history kept: %v", rc)
	}
	// Only people who cannot receive it: refused. Email not set up: refused with a reason.
	if r := e.do(khoa, "POST", "/api/v1/mailings", map[string]any{"subject": "s", "body": "b", "contact_ids": []string{quiet["id"].(string)}}, key("m2")); r.Status != 400 {
		t.Fatalf("no one to send to: %d %v", r.Status, r.Body)
	}
	e.mail.off = true
	if r := e.do(khoa, "POST", "/api/v1/mailings", mail, key("m3")); r.reason() != "email_disabled" {
		t.Fatalf("email off: %v", r.Body)
	}
}
