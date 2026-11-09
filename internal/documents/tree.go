package documents

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/auth"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// TreeContract is a contract under its customer.
type TreeContract struct {
	ID       uuid.UUID `json:"id"`
	Number   string    `json:"number"`
	Title    string    `json:"title"`
	Status   string    `json:"status"`
	SignedOn *string   `json:"signed_on"`
	Files    int       `json:"files"`
}

// TreeCustomer is a customer folder: its general files and its contracts.
type TreeCustomer struct {
	ID           uuid.UUID      `json:"id"`
	Code         string         `json:"code"`
	Name         string         `json:"name"`
	GeneralFiles int            `json:"general_files"`
	Contracts    []TreeContract `json:"contracts"`
}

// Tree handles GET /documents/tree?year=26&closed=1: customer → contracts in the
// caller's scope; active first, then newest; drafts last (REQUIREMENTS 5.12). The
// year is a filter, never a folder. File counts include only files the caller may see.
func (h *Handlers) Tree(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	year := c.Query("year")
	if year != "" && (len(year) != 2 || strings.Trim(year, "0123456789") != "") {
		response.Fail(c, apperrors.Invalid("year", "format", nil))
		return
	}
	statuses := []string{"draft", "active"}
	if c.Query("closed") == "1" {
		statuses = append(statuses, "closed")
	}
	rows, err := h.Pool.Query(ctx, `SELECT cu.id, cu.code, cu.name, c.id, coalesce(c.number, c.draft_number), c.title, c.status, c.signed_on
		FROM contracts c JOIN organizations cu ON cu.id = c.customer_id
		WHERE ($1::uuid IS NULL OR c.owner_id = $1) AND c.status = ANY($2)
		  AND ($3 = '' OR substr(c.number, 4, 2) = $3)
		ORDER BY cu.name, cu.id, (c.status = 'active') DESC, (c.status = 'draft'), c.number DESC NULLS LAST, c.created_at DESC`, p.OwnerFilter(), statuses, year)
	if err != nil {
		response.Fail(c, err)
		return
	}
	out := []*TreeCustomer{}
	byID := map[uuid.UUID]*TreeCustomer{}
	contractIndex := map[uuid.UUID]*TreeContract{}
	for rows.Next() {
		var cu TreeCustomer
		var k TreeContract
		var signed *time.Time
		if err := rows.Scan(&cu.ID, &cu.Code, &cu.Name, &k.ID, &k.Number, &k.Title, &k.Status, &signed); err != nil {
			rows.Close()
			response.Fail(c, err)
			return
		}
		if signed != nil {
			s := signed.Format(time.DateOnly)
			k.SignedOn = &s
		}
		t := byID[cu.ID]
		if t == nil {
			cu.Contracts = []TreeContract{}
			t = &cu
			byID[cu.ID] = t
			out = append(out, t)
		}
		t.Contracts = append(t.Contracts, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	for _, cu := range out {
		for i := range cu.Contracts {
			contractIndex[cu.Contracts[i].ID] = &cu.Contracts[i]
		}
	}
	// Count visible files per contract and per customer (general), applying the same rules as the lists.
	lrows, err := h.Pool.Query(ctx, `SELECT l.contract_id, l.customer_id, l.owner_type, l.doc_kind FROM attachment_links l
		WHERE l.contract_id = ANY($1) OR (l.contract_id IS NULL AND l.owner_type = 'customer')`, keys(contractIndex))
	if err != nil {
		response.Fail(c, err)
		return
	}
	general := map[uuid.UUID]int{}
	for lrows.Next() {
		var contractID, customerID *uuid.UUID
		var ownerType, kind string
		if err := lrows.Scan(&contractID, &customerID, &ownerType, &kind); err != nil {
			lrows.Close()
			response.Fail(c, err)
			return
		}
		if !Visible(p, ownerType, kind) {
			continue
		}
		if contractID != nil {
			if k := contractIndex[*contractID]; k != nil {
				k.Files++
			}
		} else if customerID != nil {
			general[*customerID]++
		}
	}
	lrows.Close()
	for _, cu := range out {
		cu.GeneralFiles = general[cu.ID]
	}
	response.OK(c, out)
}

func keys(m map[uuid.UUID]*TreeContract) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// CustomerFiles handles GET /documents/customers/:id: the customer's "Chung" folder.
func (h *Handlers) CustomerFiles(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	o, err := resolveOwner(ctx, h.Pool, p, OwnerCustomer, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	items, err := h.visibleLinks(ctx, p, `l.owner_type = 'customer' AND l.owner_id = $1`, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	var name string
	_ = h.Pool.QueryRow(ctx, `SELECT name FROM organizations WHERE id = $1`, id).Scan(&name)
	response.OK(c, gin.H{"id": id, "code": o.CustomerCode, "name": name, "files": items, "kinds": KindsFor(OwnerCustomer)})
}

// Group is one document folder inside a section (a PO with its receipts, a delivery, an invoice).
type Group struct {
	OwnerType string    `json:"owner_type"`
	OwnerID   uuid.UUID `json:"owner_id"`
	Title     string    `json:"title"`
	// Meta is what the folder line says (supplier, date, line count, amount); the client words it.
	Meta  map[string]any `json:"meta"`
	Warn  string         `json:"warn"`
	Files []FileItem     `json:"files"`
	Kinds []string       `json:"kinds"`
	// Receipts lets the upload form attach to a goods receipt shown inside its PO.
	Receipts []GroupRef `json:"receipts,omitempty"`
}

// GroupRef names a document inside a group.
type GroupRef struct {
	ID     uuid.UUID `json:"id"`
	Number string    `json:"number"`
}

// Section is one of the fixed folders 1–6.
type Section struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Docs   int     `json:"docs"`
	Files  int     `json:"files"`
	Warn   string  `json:"warn"`
	Groups []Group `json:"groups"`
}

// ContractDocs is a contract's folder.
type ContractDocs struct {
	ID                uuid.UUID `json:"id"`
	Number            string    `json:"number"`
	Title             string    `json:"title"`
	Status            string    `json:"status"`
	CustomerID        uuid.UUID `json:"customer_id"`
	CustomerCode      string    `json:"customer_code"`
	CustomerName      string    `json:"customer_name"`
	CustomerReference *string   `json:"customer_reference"`
	SignedOn          *string   `json:"signed_on"`
	FilesTotal        int       `json:"files_total"`
	Sections          []Section `json:"sections"`
}

// Contract handles GET /documents/contracts/:id.
func (h *Handlers) Contract(c *gin.Context) {
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	cd, err := h.contractDocs(c.Request.Context(), auth.Principal(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, cd)
}

// contractDocs builds the six fixed sections. Every document gets a folder even with no
// files yet, with a warning saying what should be there (REQUIREMENTS 5.12).
func (h *Handlers) contractDocs(ctx context.Context, p *access.Principal, id uuid.UUID) (*ContractDocs, error) {
	cd := &ContractDocs{ID: id}
	var signed *time.Time
	err := h.Pool.QueryRow(ctx, `SELECT coalesce(c.number, c.draft_number), c.title, c.status, cu.id, cu.code, cu.name, c.customer_reference, c.signed_on
		FROM contracts c JOIN organizations cu ON cu.id = c.customer_id WHERE c.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`, id, p.OwnerFilter()).
		Scan(&cd.Number, &cd.Title, &cd.Status, &cd.CustomerID, &cd.CustomerCode, &cd.CustomerName, &cd.CustomerReference, &signed)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound()
		}
		return nil, err
	}
	if signed != nil {
		s := signed.Format(time.DateOnly)
		cd.SignedOn = &s
	}
	all, err := h.visibleLinks(ctx, p, `l.contract_id = $1`, id)
	if err != nil {
		return nil, err
	}
	byOwner := map[uuid.UUID][]FileItem{}
	var contractLevel []FileItem
	for _, f := range all {
		if f.OwnerType == OwnerContract {
			contractLevel = append(contractLevel, f)
		} else {
			byOwner[f.OwnerID] = append(byOwner[f.OwnerID], f)
		}
	}
	cd.FilesTotal = len(all)
	has := func(files []FileItem, kinds ...string) bool {
		for _, f := range files {
			for _, k := range kinds {
				if f.Kind == k {
					return true
				}
			}
		}
		return false
	}

	// 1 and 6: files on the contract itself, split by kind.
	var main, other []FileItem
	for _, f := range contractLevel {
		if f.Kind == "signed_contract" || f.Kind == "amendment" || f.Kind == "quotation" {
			main = append(main, f)
		} else {
			other = append(other, f)
		}
	}
	s1 := Section{Key: "contract", Label: "1. Hợp đồng & phụ lục", Groups: []Group{{OwnerType: OwnerContract, OwnerID: id, Files: nonNil(main), Kinds: []string{"signed_contract", "amendment", "quotation"}}}}
	if cd.Status == "active" && !has(main, "signed_contract") {
		s1.Warn = "missing_signed_contract"
	}

	// 2: purchase orders, each with its goods receipts' files.
	s2 := Section{Key: "purchase_orders", Label: "2. Đơn mua (PO)", Groups: []Group{}}
	porows, err := h.Pool.Query(ctx, `SELECT po.id, po.number, s.name, po.status,
			coalesce((SELECT array_agg(gr.id::text || ' ' || gr.number ORDER BY gr.number) FROM goods_receipts gr WHERE gr.purchase_order_id = po.id AND gr.status = 'confirmed'), '{}')
		FROM purchase_orders po JOIN organizations s ON s.id = po.supplier_id
		WHERE po.contract_id = $1 AND po.status <> 'cancelled' ORDER BY po.number`, id)
	if err != nil {
		return nil, err
	}
	type po struct {
		id                     uuid.UUID
		number, supplier, stat string
		grs                    []string
	}
	var pos []po
	for porows.Next() {
		var x po
		if err := porows.Scan(&x.id, &x.number, &x.supplier, &x.stat, &x.grs); err != nil {
			porows.Close()
			return nil, err
		}
		pos = append(pos, x)
	}
	porows.Close()
	for _, x := range pos {
		files := append([]FileItem{}, byOwner[x.id]...)
		var refs []GroupRef
		for _, g := range x.grs {
			gid, num, _ := strings.Cut(g, " ")
			u := uuid.MustParse(gid)
			refs = append(refs, GroupRef{ID: u, Number: num})
			files = append(files, byOwner[u]...)
		}
		g := Group{OwnerType: OwnerPurchaseOrder, OwnerID: x.id, Title: x.number, Meta: map[string]any{"supplier": x.supplier, "status": x.stat}, Files: files, Kinds: KindsFor(OwnerPurchaseOrder), Receipts: refs}
		if x.stat == "confirmed" && p.Can(access.PurchaseCostRead) && !has(files, "po_signed") {
			g.Warn = "missing_po_signed"
		} else if len(x.grs) > 0 && !has(files, "cq") {
			g.Warn = "missing_cq"
		}
		s2.Groups = append(s2.Groups, g)
		s2.Files += len(files)
	}
	s2.Docs = len(pos)

	// 3: deliveries.
	s3 := Section{Key: "delivery_orders", Label: "3. Giao hàng (DO)", Groups: []Group{}}
	drows, err := h.Pool.Query(ctx, `SELECT d.id, d.number, d.status, coalesce(d.delivered_on, d.planned_on),
			(SELECT count(*) FROM delivery_order_lines WHERE delivery_order_id = d.id)
		FROM delivery_orders d WHERE d.contract_id = $1 AND d.status <> 'cancelled' ORDER BY d.number`, id)
	if err != nil {
		return nil, err
	}
	for drows.Next() {
		var did uuid.UUID
		var number, status string
		var on *time.Time
		var lines int
		if err := drows.Scan(&did, &number, &status, &on, &lines); err != nil {
			drows.Close()
			return nil, err
		}
		g := Group{OwnerType: OwnerDeliveryOrder, OwnerID: did, Title: number, Files: nonNil(byOwner[did]), Kinds: KindsFor(OwnerDeliveryOrder)}
		g.Meta = map[string]any{"status": status, "lines": lines}
		if on != nil {
			g.Meta["date"] = on.Format(time.DateOnly)
		}
		if status == "confirmed" && !has(g.Files, "delivery_note") {
			g.Warn = "missing_delivery_note"
		}
		s3.Groups = append(s3.Groups, g)
		s3.Files += len(g.Files)
	}
	drows.Close()
	s3.Docs = len(s3.Groups)

	// 4 and 5: money documents, only with finance access.
	s4 := Section{Key: "sales_invoices", Label: "4. Hóa đơn bán", Groups: []Group{}}
	s5 := Section{Key: "payments", Label: "5. Thanh toán", Groups: []Group{}}
	if p.Can(access.FinanceRead) {
		irows, err := h.Pool.Query(ctx, `SELECT id, number, legal_number, description, amount_total::text, currency FROM sales_invoices
			WHERE contract_id = $1 AND status <> 'cancelled' ORDER BY number`, id)
		if err != nil {
			return nil, err
		}
		for irows.Next() {
			var iid uuid.UUID
			var number, legal, desc, total, cur string
			if err := irows.Scan(&iid, &number, &legal, &desc, &total, &cur); err != nil {
				irows.Close()
				return nil, err
			}
			g := Group{OwnerType: OwnerSalesInvoice, OwnerID: iid, Title: legal + " · " + number, Meta: map[string]any{"description": desc, "amount": total, "currency": cur}, Files: nonNil(byOwner[iid]), Kinds: KindsFor(OwnerSalesInvoice)}
			if !has(g.Files, "sales_invoice") {
				g.Warn = "missing_invoice_file"
			}
			s4.Groups = append(s4.Groups, g)
			s4.Files += len(g.Files)
		}
		irows.Close()
		s4.Docs = len(s4.Groups)
		rrows, err := h.Pool.Query(ctx, `SELECT DISTINCT r.id, r.number, r.amount::text, r.currency, r.received_on FROM receipts r
			JOIN receipt_allocations a ON a.receipt_id = r.id JOIN payment_milestones m ON m.id = a.milestone_id
			WHERE m.contract_id = $1 AND r.status <> 'cancelled' ORDER BY r.number`, id)
		if err != nil {
			return nil, err
		}
		var rids []uuid.UUID
		for rrows.Next() {
			var rid uuid.UUID
			var number, amount, cur string
			var on time.Time
			if err := rrows.Scan(&rid, &number, &amount, &cur, &on); err != nil {
				rrows.Close()
				return nil, err
			}
			rids = append(rids, rid)
			s5.Groups = append(s5.Groups, Group{OwnerType: OwnerReceipt, OwnerID: rid, Title: number, Meta: map[string]any{"amount": amount, "currency": cur, "date": on.Format(time.DateOnly)}, Kinds: KindsFor(OwnerReceipt)})
		}
		rrows.Close()
		if len(rids) > 0 {
			// Receipts belong to a customer, not one contract, so their files are looked up by owner.
			rf, err := h.visibleLinks(ctx, p, `l.owner_type = 'receipt' AND l.owner_id = ANY($1)`, rids)
			if err != nil {
				return nil, err
			}
			for i := range s5.Groups {
				s5.Groups[i].Files = []FileItem{}
				for _, f := range rf {
					if f.OwnerID == s5.Groups[i].OwnerID {
						s5.Groups[i].Files = append(s5.Groups[i].Files, f)
					}
				}
				s5.Files += len(s5.Groups[i].Files)
			}
		}
		s5.Docs = len(s5.Groups)
	}

	s6 := Section{Key: "other", Label: "6. Khác", Groups: []Group{{OwnerType: OwnerContract, OwnerID: id, Files: nonNil(other), Kinds: []string{"minutes", "correspondence", "photo", "other"}}}}
	s1.Files, s6.Files = len(main), len(other)
	for _, s := range []*Section{&s2, &s3, &s4} {
		for _, g := range s.Groups {
			if g.Warn != "" {
				s.Warn = g.Warn + ":" + g.Title
				break
			}
		}
	}
	cd.Sections = []Section{s1, s2, s3}
	if p.Can(access.FinanceRead) {
		cd.Sections = append(cd.Sections, s4, s5)
	}
	cd.Sections = append(cd.Sections, s6)
	return cd, nil
}

func nonNil(f []FileItem) []FileItem {
	if f == nil {
		return []FileItem{}
	}
	sort.SliceStable(f, func(i, j int) bool { return f[i].UploadedAt.Before(f[j].UploadedAt) })
	return f
}
