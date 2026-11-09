package documents

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gabriel-vasile/mimetype"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"navo-api/internal/access"
	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/files"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

// MaxUpload is the largest file accepted (a scanned contract or a phone photo fits easily).
const MaxUpload = 25 << 20

// Accepted content types: documents, scans, photos and e-invoice XML.
var acceptedTypes = map[string]bool{
	"application/pdf": true, "image/jpeg": true, "image/png": true, "image/webp": true, "image/heic": true, "image/heif": true,
	"text/xml": true, "application/xml": true, "text/plain": true, "text/csv": true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
	"application/vnd.ms-excel": true, "application/msword": true, "application/x-ole-storage": true,
}

// Handlers serve files and the documents tree.
type Handlers struct {
	Pool  *pgxpool.Pool
	Store files.Store
}

// owner is a document a file can be attached to, resolved within the caller's scope.
type owner struct {
	Type         string
	ID           uuid.UUID
	CustomerID   *uuid.UUID
	CustomerCode string
	ContractID   *uuid.UUID
	Number       string
}

// resolveOwner loads a customer or document the caller may see; anything else is not found.
func resolveOwner(ctx context.Context, q db.Querier, p *access.Principal, ownerType string, id uuid.UUID) (*owner, error) {
	o := &owner{Type: ownerType, ID: id}
	var sql string
	switch ownerType {
	case OwnerCustomer:
		sql = `SELECT o.id, o.code, NULL::uuid, o.code FROM organizations o WHERE o.id = $1 AND o.is_customer AND ($2::uuid IS NULL OR true)`
	case OwnerContract:
		sql = `SELECT c.customer_id, cu.code, c.id, coalesce(c.number, c.draft_number) FROM contracts c JOIN organizations cu ON cu.id = c.customer_id
			WHERE c.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`
	case OwnerPurchaseOrder, OwnerGoodsReceipt, OwnerDeliveryOrder:
		table := map[string]string{OwnerPurchaseOrder: "purchase_orders", OwnerGoodsReceipt: "goods_receipts", OwnerDeliveryOrder: "delivery_orders"}[ownerType]
		sql = `SELECT c.customer_id, cu.code, c.id, d.number FROM ` + table + ` d JOIN contracts c ON c.id = d.contract_id JOIN organizations cu ON cu.id = c.customer_id
			WHERE d.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`
	case OwnerSalesInvoice:
		sql = `SELECT c.customer_id, cu.code, c.id, d.number FROM sales_invoices d JOIN contracts c ON c.id = d.contract_id JOIN organizations cu ON cu.id = c.customer_id
			WHERE d.id = $1 AND ($2::uuid IS NULL OR c.owner_id = $2)`
	case OwnerNote:
		// Notes: the author, whoever owns the note's contract, and people who see everyone.
		sql = `SELECT n.customer_id, cu.code, n.contract_id, 'note' FROM notes n JOIN organizations cu ON cu.id = n.customer_id
			LEFT JOIN contracts c ON c.id = n.contract_id
			WHERE n.id = $1 AND ($2::uuid IS NULL OR n.author_id = $2 OR c.owner_id = $2)`
	case OwnerReceipt:
		sql = `SELECT d.customer_id, cu.code, NULL::uuid, d.number FROM receipts d JOIN organizations cu ON cu.id = d.customer_id WHERE d.id = $1 AND $2::uuid IS NULL`
	case OwnerPurchaseInvoice:
		sql = `SELECT NULL::uuid, s.code, NULL::uuid, d.number FROM purchase_invoices d JOIN organizations s ON s.id = d.supplier_id WHERE d.id = $1 AND $2::uuid IS NULL`
	case OwnerPayment:
		sql = `SELECT NULL::uuid, s.code, NULL::uuid, d.number FROM payments d JOIN organizations s ON s.id = d.supplier_id WHERE d.id = $1 AND $2::uuid IS NULL`
	default:
		return nil, apperrors.Invalid("owner_type", "one_of", nil)
	}
	scope := p.OwnerFilter()
	if ownerType == OwnerCustomer {
		scope = nil // customers are shared master data
	}
	if financeOwner(ownerType) {
		if !p.Can(access.FinanceRead) {
			return nil, apperrors.NotFound()
		}
		scope = nil // finance sees every contract's money documents
	}
	if err := q.QueryRow(ctx, sql, id, scope).Scan(&o.CustomerID, &o.CustomerCode, &o.ContractID, &o.Number); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.NotFound()
		}
		return nil, err
	}
	if ownerType == OwnerCustomer {
		o.CustomerID = &o.ID
	}
	return o, nil
}

// FileItem is one attached file as a list shows it.
type FileItem struct {
	LinkID     uuid.UUID `json:"link_id"`
	FileID     uuid.UUID `json:"file_id"`
	Name       string    `json:"name"`
	Ext        string    `json:"ext"`
	Kind       string    `json:"kind"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
	UploadedBy string    `json:"uploaded_by"`
	OwnerType  string    `json:"owner_type"`
	OwnerID    uuid.UUID `json:"owner_id"`
	// SharedWith names the other documents the same file is attached to ("dùng chung với PO-…").
	SharedWith []string `json:"shared_with"`
	CanRemove  bool     `json:"can_remove"`
}

func ext(name string) string {
	e := strings.ToUpper(strings.TrimPrefix(filepath.Ext(name), "."))
	if len(e) > 5 {
		return ""
	}
	return e
}

// Upload handles POST /files (multipart: file, owner_type, owner_id, doc_kind): stores
// the bytes once and attaches them to the document, which decides the folder.
func (h *Handlers) Upload(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxUpload+1<<20)
	ownerType, docKind := c.PostForm("owner_type"), c.PostForm("doc_kind")
	var errs apperrors.FieldErrors
	ownerID := httpx.UUID(&errs, "owner_id", c.PostForm("owner_id"))
	if !allowed(ownerType, docKind) {
		errs.Add("doc_kind", "one_of", map[string]any{"values": KindsFor(ownerType)})
	}
	fh, err := c.FormFile("file")
	if err != nil {
		errs.Add("file", "required", nil)
	} else if fh.Size > MaxUpload {
		errs.Add("file", "file_too_large", map[string]any{"max_mb": MaxUpload >> 20})
	} else if name := strings.TrimSpace(filepath.Base(fh.Filename)); name == "" || len(name) > 255 {
		errs.Add("file", "file_name", nil)
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	if !Visible(p, ownerType, docKind) {
		response.Fail(c, apperrors.Forbidden("doc_kind_"+docKind))
		return
	}
	o, err := resolveOwner(ctx, h.Pool, p, ownerType, ownerID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	src, err := fh.Open()
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer src.Close()
	head := make([]byte, 3072)
	n, _ := io.ReadFull(src, head)
	mt := mimetype.Detect(head[:n])
	ctype := mt.String()
	if i := strings.IndexByte(ctype, ';'); i > 0 {
		ctype = ctype[:i]
	}
	if !acceptedTypes[ctype] && !(ctype == "application/zip" && (strings.HasSuffix(strings.ToLower(fh.Filename), ".xlsx") || strings.HasSuffix(strings.ToLower(fh.Filename), ".docx"))) {
		response.Fail(c, apperrors.Invalid("file", "file_type", map[string]any{"type": ctype}))
		return
	}
	if ctype == "application/zip" || ctype == "application/x-ole-storage" {
		ctype = mime.TypeByExtension(strings.ToLower(filepath.Ext(fh.Filename)))
	}
	fileID := uuid.New()
	size, sum, err := h.Store.Save(fileID, io.MultiReader(strings.NewReader(string(head[:n])), src), MaxUpload)
	if errors.Is(err, files.ErrTooLarge) {
		response.Fail(c, apperrors.Invalid("file", "file_too_large", map[string]any{"max_mb": MaxUpload >> 20}))
		return
	}
	if err != nil {
		response.Fail(c, err)
		return
	}
	name := strings.TrimSpace(filepath.Base(fh.Filename))
	var item FileItem
	err = db.InTx(ctx, h.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO files (id, original_name, content_type, size_bytes, sha256, uploaded_by) VALUES ($1, $2, $3, $4, $5, $6)`,
			fileID, name, ctype, size, sum, p.UserID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO attachment_links (file_id, owner_type, owner_id, customer_id, contract_id, doc_kind, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at`, fileID, ownerType, ownerID, o.CustomerID, o.ContractID, docKind, p.UserID).
			Scan(&item.LinkID, &item.UploadedAt); err != nil {
			return err
		}
		return audit.Record(ctx, tx, "attach", "file", fileID, map[string]any{"name": name, "owner_type": ownerType, "owner": o.Number, "kind": docKind, "size": size})
	})
	if err != nil {
		h.Store.Remove(fileID)
		response.Fail(c, err)
		return
	}
	item.FileID, item.Name, item.Ext, item.Kind, item.Size = fileID, name, ext(name), docKind, size
	item.UploadedBy, item.OwnerType, item.OwnerID, item.SharedWith, item.CanRemove = p.Name, ownerType, ownerID, []string{}, true
	c.JSON(http.StatusCreated, gin.H{"request_id": c.GetString("request_id"), "status": http.StatusCreated, "success": true, "data": item})
}

// link is one attachment with what is needed to check and name it.
type link struct {
	FileItem
	CustomerID  *uuid.UUID
	ContractID  *uuid.UUID
	CreatedBy   uuid.UUID
	ContentType string
}

const linkSelect = `SELECT l.id, f.id, f.original_name, l.doc_kind, f.size_bytes, l.created_at, u.full_name, l.owner_type, l.owner_id,
	l.customer_id, l.contract_id, l.created_by, f.content_type,
	coalesce((SELECT array_agg(DISTINCT x.owner_type || ':' || x.owner_id::text) FROM attachment_links x WHERE x.file_id = f.id AND x.id <> l.id), '{}')
	FROM attachment_links l JOIN files f ON f.id = l.file_id JOIN users u ON u.id = l.created_by`

func scanLinks(rows pgx.Rows) ([]link, map[uuid.UUID][]string, error) {
	out := []link{}
	shared := map[uuid.UUID][]string{}
	for rows.Next() {
		var l link
		var others []string
		if err := rows.Scan(&l.LinkID, &l.FileID, &l.Name, &l.Kind, &l.Size, &l.UploadedAt, &l.UploadedBy, &l.OwnerType, &l.OwnerID,
			&l.CustomerID, &l.ContractID, &l.CreatedBy, &l.ContentType, &others); err != nil {
			return nil, nil, err
		}
		l.Ext = ext(l.Name)
		shared[l.LinkID] = others
		out = append(out, l)
	}
	return out, shared, rows.Err()
}

// numbersOf names the documents in "type:id" references, for "dùng chung với …".
func numbersOf(ctx context.Context, q db.Querier, refs []string) []string {
	out := []string{}
	for _, r := range refs {
		t, id, ok := strings.Cut(r, ":")
		if !ok {
			continue
		}
		table := map[string]string{OwnerContract: "contracts", OwnerPurchaseOrder: "purchase_orders", OwnerGoodsReceipt: "goods_receipts",
			OwnerDeliveryOrder: "delivery_orders", OwnerSalesInvoice: "sales_invoices", OwnerPurchaseInvoice: "purchase_invoices",
			OwnerReceipt: "receipts", OwnerPayment: "payments"}[t]
		if table == "" {
			continue
		}
		col := "number"
		if t == OwnerContract {
			col = "coalesce(number, draft_number)"
		}
		var n string
		if err := q.QueryRow(ctx, `SELECT `+col+` FROM `+table+` WHERE id = $1`, id).Scan(&n); err == nil {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (h *Handlers) visibleLinks(ctx context.Context, p *access.Principal, where string, args ...any) ([]FileItem, error) {
	rows, err := h.Pool.Query(ctx, linkSelect+` WHERE `+where+` ORDER BY l.created_at, f.original_name`, args...)
	if err != nil {
		return nil, err
	}
	links, shared, err := scanLinks(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []FileItem{}
	for _, l := range links {
		if !Visible(p, l.OwnerType, l.Kind) {
			continue
		}
		l.SharedWith = numbersOf(ctx, h.Pool, shared[l.LinkID])
		l.CanRemove = l.CreatedBy == p.UserID || p.Can(access.ContractsWrite)
		out = append(out, l.FileItem)
	}
	return out, nil
}

// Download handles GET /files/:id?link=: the bytes of a file the caller may see through
// that link, named {customer}_{document}_{kind}_{date}.{ext} (REQUIREMENTS 5.12).
func (h *Handlers) Download(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	fileID, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	linkID, err := uuid.Parse(c.Query("link"))
	if err != nil {
		response.Fail(c, apperrors.NotFound())
		return
	}
	rows, err := h.Pool.Query(ctx, linkSelect+` WHERE l.id = $1 AND l.file_id = $2`, linkID, fileID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	links, _, err := scanLinks(rows)
	rows.Close()
	if err != nil {
		response.Fail(c, err)
		return
	}
	if len(links) == 0 || !Visible(p, links[0].OwnerType, links[0].Kind) {
		response.Fail(c, apperrors.NotFound())
		return
	}
	l := links[0]
	o, err := resolveOwner(ctx, h.Pool, p, l.OwnerType, l.OwnerID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	f, err := h.Store.Open(fileID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer f.Close()
	disposition := "attachment"
	if c.Query("inline") == "1" && (l.ContentType == "application/pdf" || strings.HasPrefix(l.ContentType, "image/")) {
		disposition = "inline"
	}
	c.Header("Content-Type", l.ContentType)
	c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": DownloadName(o.CustomerCode, o.Number, l.Kind, l.UploadedAt, l.Name)}))
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, f)
}

// DownloadName builds the standard file name, ASCII only so every system keeps it.
func DownloadName(customerCode, number, docKind string, at time.Time, original string) string {
	k := kinds[docKind].ascii
	if k == "" {
		k = "Khac"
	}
	parts := []string{customerCode}
	if number != customerCode {
		parts = append(parts, number)
	}
	parts = append(parts, k, at.Format(time.DateOnly))
	return safeName(strings.Join(parts, "_")) + strings.ToLower(filepath.Ext(original))
}

var fold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// safeName drops Vietnamese diacritics and characters Windows forbids, and caps the length.
func safeName(s string) string {
	s, _, _ = transform.String(fold, s)
	s = strings.NewReplacer("đ", "d", "Đ", "D").Replace(s)
	b := strings.Builder{}
	for _, r := range s {
		switch {
		case r < 32 || strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		case r > 126:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

// Unlink handles DELETE /attachment-links/:id: the file leaves that document; the
// bytes stay (other links and backups may still need them).
func (h *Handlers) Unlink(c *gin.Context) {
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "attachment_links.delete", false, 200, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var ownerType, docKind, name string
		var ownerID, fileID, createdBy uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT l.owner_type, l.owner_id, l.doc_kind, l.file_id, l.created_by, f.original_name
			FROM attachment_links l JOIN files f ON f.id = l.file_id WHERE l.id = $1 FOR UPDATE OF l`, id).
			Scan(&ownerType, &ownerID, &docKind, &fileID, &createdBy, &name); err != nil {
			return nil, err
		}
		if !Visible(p, ownerType, docKind) {
			return nil, apperrors.NotFound()
		}
		if _, err := resolveOwner(ctx, tx, p, ownerType, ownerID); err != nil {
			return nil, err
		}
		if createdBy != p.UserID && !p.Can(access.ContractsWrite) {
			return nil, apperrors.Forbidden("remove_file")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM attachment_links WHERE id = $1`, id); err != nil {
			return nil, err
		}
		return gin.H{"id": id, "removed": true}, audit.Record(ctx, tx, "detach", "file", fileID, map[string]any{"name": name, "owner_type": ownerType, "kind": docKind})
	})
}

// zipEntry is one file placed in the archive's folder structure.
type zipEntry struct {
	folder string
	item   FileItem
}

// Zip handles GET /documents/contracts/:id/zip: the contract's folders as real
// folders, only the files the caller may see.
func (h *Handlers) Zip(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	id, err := httpx.PathID(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	cd, err := h.contractDocs(ctx, p, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	root := safeName(fmt.Sprintf("%s - %s", cd.Number, cd.Title))
	var entries []zipEntry
	for _, s := range cd.Sections {
		for _, g := range s.Groups {
			folder := path.Join(root, safeName(s.Label))
			if g.Title != "" {
				folder = path.Join(folder, safeName(g.Title))
			}
			for _, f := range g.Files {
				entries = append(entries, zipEntry{folder, f})
			}
		}
	}
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeName(cd.CustomerCode+"_"+cd.Number) + ".zip"}))
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)
	zw := zip.NewWriter(c.Writer)
	used := map[string]int{}
	for _, e := range entries {
		base := strings.TrimSuffix(safeName(e.item.Name), ".")
		name := path.Join(e.folder, base)
		if n := used[name]; n > 0 {
			ext := path.Ext(base)
			name = path.Join(e.folder, fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(base, ext), n+1, ext))
		}
		used[path.Join(e.folder, base)]++
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: e.item.UploadedAt})
		if err != nil {
			break
		}
		f, err := h.Store.Open(e.item.FileID)
		if err != nil {
			continue
		}
		_, _ = io.Copy(w, f)
		f.Close()
	}
	_ = zw.Close()
}
