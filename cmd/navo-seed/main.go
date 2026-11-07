// Command navo-seed fills an empty development database with demo users and
// data matching the mockups. It drives the real HTTP handlers in-process, so
// demo data passes the same validation, numbering and audit as real use.
//
// It refuses to run when organizations already exist, and refuses databases
// whose name does not contain "dev" or "demo".
//
//	NAVO_SEED_PASSWORD=dev-password-123 go run ./cmd/navo-seed
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"

	"navo-api/internal/config"
	"navo-api/internal/db"
	"navo-api/internal/router"
	"navo-api/internal/users"
)

type client struct {
	base, token string
	n           int
}

func (c *client) call(method, path string, body any, idempotent bool) map[string]any {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, c.base+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if idempotent {
		c.n++
		req.Header.Set("Idempotency-Key", fmt.Sprintf("seed-%d-%d", time.Now().UnixNano(), c.n))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		fail("%s %s: %d %v", method, path, resp.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	return data
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "seed failed: "+format+"\n", args...)
	os.Exit(1)
}

func id(m map[string]any) string { return m["id"].(string) }

func line(contract map[string]any, no int) string {
	for _, l := range contract["lines"].([]any) {
		m := l.(map[string]any)
		if int(m["line_no"].(float64)) == no {
			return m["contract_line_id"].(string)
		}
	}
	fail("line %d not found", no)
	return ""
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		fail("%v", err)
	}
	password := os.Getenv("NAVO_SEED_PASSWORD")
	if len(password) < 10 {
		fail("set NAVO_SEED_PASSWORD (at least 10 characters) for the demo accounts")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fail("%v", err)
	}
	defer pool.Close()
	var dbName string
	var orgs int
	if err := pool.QueryRow(ctx, `SELECT current_database(), (SELECT count(*) FROM organizations)`).Scan(&dbName, &orgs); err != nil {
		fail("%v (did you run make migrate?)", err)
	}
	if !strings.Contains(dbName, "dev") && !strings.Contains(dbName, "demo") {
		fail("refusing to seed %q: only databases named *dev* or *demo*", dbName)
	}
	if orgs > 0 {
		fail("database %q already has data; seed only runs on an empty database", dbName)
	}

	accounts := []users.Input{
		{Email: "admin@congty.vn", FullName: "Vũ Hoàng Nam", Roles: []string{"admin"}},
		{Email: "lan@congty.vn", FullName: "Đỗ Thị Lan", Roles: []string{"leader"}},
		{Email: "mai@congty.vn", FullName: "Phan Ngọc Mai", Roles: []string{"accounting"}},
		{Email: "khoa@congty.vn", FullName: "Trần Minh Khoa", Roles: []string{"sales"}},
		{Email: "ha@congty.vn", FullName: "Nguyễn Thu Hà", Roles: []string{"sales"}},
	}
	err = db.InTx(ctx, pool, func(tx pgx.Tx) error {
		for _, a := range accounts {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = lower($1))`, a.Email).Scan(&exists); err != nil {
				return err
			}
			if exists {
				fmt.Println("user exists, kept:", a.Email)
				continue
			}
			a.Password = password
			if err := a.Validate(); err != nil {
				return err
			}
			if _, err := users.Create(ctx, tx, a, nil); err != nil {
				return err
			}
			fmt.Println("user created:", a.Email, a.Roles)
		}
		return nil
	})
	if err != nil {
		fail("users: %v", err)
	}

	srv := httptest.NewServer(router.New(cfg, pool))
	defer srv.Close()
	login := func(email string) *client {
		c := &client{base: srv.URL}
		c.token = c.call("POST", "/api/v1/auth/login", map[string]any{"email": email, "password": password, "client": "mobile"}, false)["token"].(string)
		return c
	}
	khoa, ha := login("khoa@congty.vn"), login("ha@congty.vn")

	org := func(c *client, code, name string, customer, supplier bool) map[string]any {
		return c.call("POST", "/api/v1/organizations", map[string]any{"code": code, "name": name, "is_customer": customer, "is_supplier": supplier}, false)
	}
	stra := org(khoa, "STRA", "Lọc hóa dầu Sông Trà", true, false)
	dpn := org(khoa, "DPN", "Đạm Phương Nam", true, false)
	org(khoa, "DGCD", "Điện gió Cửa Đại", true, false)
	org(khoa, "KDH", "Khí Đông Hải", true, false)
	dga := org(khoa, "DGA", "Thiết bị Đông Á", false, true)
	kl := org(khoa, "KL", "Kim Long", false, true)

	prod := func(name, unit, code string) map[string]any {
		return khoa.call("POST", "/api/v1/products", map[string]any{"name": name, "unit": unit, "manufacturer_code": code}, false)
	}
	valve50 := prod("Van cầu DN50 PN40", "bộ", "VC-50-40")
	tempTx := prod("Transmitter nhiệt độ PT100", "chiếc", "TT-100-B")
	pressTx := prod("Transmitter áp suất 0–40 bar", "chiếc", "PT-4020-A")
	gasket := prod("Gioăng xoắn DN100", "cái", "SW-100-316")
	pump := prod("Bơm định lượng 120 L/h", "bộ", "DP-120")
	valve80 := prod("Van cầu DN80 PN40", "bộ", "VC-80-40")

	l := func(p map[string]any, qty, price, due string, name ...string) map[string]any {
		m := map[string]any{"product_id": id(p), "quantity": qty, "unit_price": price, "due_on": due}
		if len(name) > 0 {
			m["name_on_contract"] = name[0]
		}
		return m
	}
	// Sông Trà: the AC-01 contract from the mockups.
	k := khoa.call("POST", "/api/v1/contracts", map[string]any{"customer_id": id(stra), "title": "Thiết bị đo và van · bảo dưỡng tổng thể 2027",
		"currency": "VND", "customer_reference": "PR-5520/2026", "lines": []any{
			l(valve50, "24", "18500000", "2026-10-15"),
			l(tempTx, "40", "9800000", "2026-11-30"),
			l(pressTx, "100", "24000000", "2026-12-15", "Bộ chuyển đổi áp suất 0-40bar, 4-20mA, HART"),
			l(valve50, "16", "19200000", "2026-12-15"),
			l(gasket, "200", "420000", "2026-11-30"),
			l(pump, "4", "165000000", "2026-12-15"),
		}}, false)
	k = khoa.call("POST", "/api/v1/contracts/"+id(k)+"/activate", map[string]any{"signed_on": "2026-08-12"}, true)
	kid := id(k)

	po := func(c *client, contractID, supplier string, lines ...map[string]any) map[string]any {
		p := c.call("POST", "/api/v1/contracts/"+contractID+"/purchase-orders", map[string]any{"supplier_id": supplier, "currency": "USD", "lines": lines}, false)
		return c.call("POST", "/api/v1/purchase-orders/"+id(p)+"/confirm", nil, true)
	}
	pol := func(lineID, qty, price, name, eta string) map[string]any {
		return map[string]any{"contract_line_id": lineID, "quantity": qty, "unit_price": price, "supplier_item_name": name, "expected_on": eta}
	}
	receive := func(c *client, p map[string]any, on string, qty ...[2]string) {
		var lines []any
		for i, q := range qty {
			lines = append(lines, map[string]any{"purchase_order_line_id": p["lines"].([]any)[i].(map[string]any)["id"], "accepted_quantity": q[0], "rejected_quantity": q[1]})
		}
		gr := c.call("POST", "/api/v1/purchase-orders/"+id(p)+"/goods-receipts", map[string]any{"received_on": on, "lines": lines}, false)
		c.call("POST", "/api/v1/goods-receipts/"+id(gr)+"/confirm", nil, true)
	}
	deliver := func(c *client, contractID, lineID, qty, on string) {
		d := c.call("POST", "/api/v1/contracts/"+contractID+"/delivery-orders", map[string]any{"lines": []any{map[string]any{"contract_line_id": lineID, "quantity": qty}}}, false)
		c.call("POST", "/api/v1/delivery-orders/"+id(d)+"/confirm", map[string]any{"delivered_on": on}, true)
	}
	plan := func(c *client, contractID, lineID, qty, on, firmness string) {
		c.call("POST", "/api/v1/contracts/"+contractID+"/delivery-orders", map[string]any{"planned_on": on, "firmness": firmness,
			"lines": []any{map[string]any{"contract_line_id": lineID, "quantity": qty}}}, false)
	}

	// Lines 1, 2, 5: ordered and received in full.
	p0 := po(khoa, kid, id(dga), pol(line(k, 1), "24", "720", "Globe valve DN50 PN40 WCB", "2026-09-10"),
		pol(line(k, 2), "40", "380", "PT100 temperature transmitter", "2026-09-10"), pol(line(k, 5), "200", "15.5", "Spiral wound gasket DN100", "2026-09-10"))
	receive(khoa, p0, "2026-09-12", [2]string{"24", "0"}, [2]string{"40", "0"}, [2]string{"200", "0"})
	deliver(khoa, kid, line(k, 1), "24", "2026-10-10")
	deliver(khoa, kid, line(k, 2), "20", "2026-10-20")
	deliver(khoa, kid, line(k, 5), "120", "2026-10-25")
	plan(khoa, kid, line(k, 2), "20", "2026-11-18", "firm")
	plan(khoa, kid, line(k, 5), "80", "2026-11-10", "firm")
	// Line 3 (AC-01): PO 60 + 40, received 50 + 20 (2 rejected), delivered 30 + 20, no plan for the remaining 50.
	p1 := po(khoa, kid, id(dga), pol(line(k, 3), "60", "980.50", "Pressure transmitter PT-4020-A, 0..40 bar", "2026-09-20"))
	p2 := po(khoa, kid, id(kl), pol(line(k, 3), "40", "975", "PT4020A transmitter", "2026-10-10"))
	receive(khoa, p1, "2026-09-25", [2]string{"50", "0"})
	receive(khoa, p2, "2026-10-14", [2]string{"20", "2"})
	deliver(khoa, kid, line(k, 3), "30", "2026-10-20")
	deliver(khoa, kid, line(k, 3), "20", "2026-10-28")
	// Line 4: ordered, goods late (expected 02/11), tentative plan.
	po(khoa, kid, id(kl), pol(line(k, 4), "16", "760", "Globe valve DN50 (lot 2)", "2026-11-02"))
	plan(khoa, kid, line(k, 4), "16", "2026-12-05", "tentative")
	// Line 6: only 2 of 4 pumps ordered, no plan.
	po(khoa, kid, id(dga), pol(line(k, 6), "2", "6400", "Metering pump 120 L/h, 10 bar, SS316", "2026-12-01"))

	// Đạm Phương Nam (Nguyễn Thu Hà): overdue delivery with a firm plan.
	k2 := ha.call("POST", "/api/v1/contracts", map[string]any{"customer_id": id(dpn), "title": "Van công nghệ xưởng Urea", "currency": "VND",
		"lines": []any{l(valve80, "30", "26000000", "2026-10-30")}}, false)
	k2 = ha.call("POST", "/api/v1/contracts/"+id(k2)+"/activate", map[string]any{"signed_on": "2026-07-01"}, true)
	p3 := po(ha, id(k2), id(dga), pol(line(k2, 1), "30", "1050", "Globe valve DN80 PN40", "2026-10-01"))
	receive(ha, p3, "2026-10-03", [2]string{"30", "0"})
	deliver(ha, id(k2), line(k2, 1), "18", "2026-10-05")
	plan(ha, id(k2), line(k2, 1), "12", "2026-11-09", "firm")

	// A draft contract waiting to be signed.
	khoa.call("POST", "/api/v1/contracts", map[string]any{"customer_id": id(stra), "title": "Bơm và phụ tùng (nháp)", "currency": "VND",
		"lines": []any{l(pump, "2", "170000000", "2027-03-31")}}, false)

	fmt.Println("demo data created in", dbName, "— contracts", k["number"], "and", k2["number"])
	fmt.Println("sign in with any of", "admin@ lan@ mai@ khoa@ ha@ congty.vn", "and NAVO_SEED_PASSWORD")
}
