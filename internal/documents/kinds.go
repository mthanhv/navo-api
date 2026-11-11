// Package documents serves the documents tree: files attached to customers and to
// contract documents, shown customer → contract → sections 1–6 (REQUIREMENTS 5.12).
package documents

import "navo-api/internal/access"

func canWriteFiles(p *access.Principal) bool {
	return p.Can(access.ContractsWrite) || p.Can(access.FinanceWrite)
}

// Owner types a file can be attached to.
const (
	OwnerCustomer        = "customer"
	OwnerContract        = "contract"
	OwnerPurchaseOrder   = "purchase_order"
	OwnerGoodsReceipt    = "goods_receipt"
	OwnerDeliveryOrder   = "delivery_order"
	OwnerSalesInvoice    = "sales_invoice"
	OwnerPurchaseInvoice = "purchase_invoice"
	OwnerReceipt         = "receipt"
	OwnerPayment         = "payment"
	OwnerNote            = "note"
)

// kind describes one document kind: where it may be attached, who may see it, and
// its plain-ASCII name for downloaded file names (STRA_DO-26-0142-02_BienBanGiaoNhan_2026-10-28.pdf).
type kind struct {
	owners []string
	need   access.Permission // extra permission to see the file at all (AC-11); "" = none
	ascii  string
}

var kinds = map[string]kind{
	"signed_contract":       {owners: []string{OwnerContract}, ascii: "HopDong"},
	"amendment":             {owners: []string{OwnerContract}, ascii: "PhuLuc"},
	"quotation":             {owners: []string{OwnerContract}, ascii: "BaoGia"},
	"customer_order":        {owners: []string{OwnerContract}, ascii: "DonHangKhach"},
	"minutes":               {owners: []string{OwnerContract, OwnerCustomer}, ascii: "BienBan"},
	"correspondence":        {owners: []string{OwnerContract, OwnerCustomer}, ascii: "ThuTu"},
	"profile":               {owners: []string{OwnerCustomer}, ascii: "HoSoNangLuc"},
	"nda":                   {owners: []string{OwnerCustomer}, ascii: "NDA"},
	"po_signed":             {owners: []string{OwnerPurchaseOrder}, need: access.PurchaseCostRead, ascii: "DonMua"},
	"supplier_confirmation": {owners: []string{OwnerPurchaseOrder}, need: access.PurchaseCostRead, ascii: "XacNhanNCC"},
	"supplier_invoice":      {owners: []string{OwnerPurchaseOrder, OwnerPurchaseInvoice}, need: access.PurchaseCostRead, ascii: "HoaDonNCC"},
	"packing_list":          {owners: []string{OwnerPurchaseOrder, OwnerGoodsReceipt}, ascii: "PackingList"},
	"receipt_note":          {owners: []string{OwnerGoodsReceipt}, ascii: "PhieuNhan"},
	"co":                    {owners: []string{OwnerPurchaseOrder, OwnerGoodsReceipt, OwnerDeliveryOrder}, ascii: "CO"},
	"cq":                    {owners: []string{OwnerPurchaseOrder, OwnerGoodsReceipt, OwnerDeliveryOrder}, ascii: "CQ"},
	"delivery_note":         {owners: []string{OwnerDeliveryOrder}, ascii: "BienBanGiaoNhan"},
	"acceptance":            {owners: []string{OwnerDeliveryOrder}, ascii: "BienBanNghiemThu"},
	"photo":                 {owners: []string{OwnerContract, OwnerGoodsReceipt, OwnerDeliveryOrder, OwnerNote}, ascii: "Anh"},
	"sales_invoice":         {owners: []string{OwnerSalesInvoice}, need: access.FinanceRead, ascii: "HoaDonBan"},
	"bank":                  {owners: []string{OwnerReceipt, OwnerPayment}, need: access.FinanceRead, ascii: "ChungTuNganHang"},
	"other":                 {owners: []string{OwnerCustomer, OwnerContract, OwnerPurchaseOrder, OwnerGoodsReceipt, OwnerDeliveryOrder, OwnerSalesInvoice, OwnerPurchaseInvoice, OwnerReceipt, OwnerPayment}, ascii: "Khac"},
}

// financeOwner reports owner types that only finance may see, whatever the kind.
func financeOwner(t string) bool {
	return t == OwnerSalesInvoice || t == OwnerPurchaseInvoice || t == OwnerReceipt || t == OwnerPayment
}

// Visible reports whether p may see a file attached as (ownerType, docKind); the
// document itself must already be in p's scope.
func Visible(p *access.Principal, ownerType, docKind string) bool {
	if financeOwner(ownerType) && !p.Can(access.FinanceRead) {
		return false
	}
	k, ok := kinds[docKind]
	if !ok {
		return false
	}
	if ownerType == OwnerPurchaseInvoice && !p.Can(access.PurchaseCostRead) {
		return false
	}
	return k.need == "" || p.Can(k.need)
}

func allowed(ownerType, docKind string) bool {
	k, ok := kinds[docKind]
	if !ok {
		return false
	}
	for _, o := range k.owners {
		if o == ownerType {
			return true
		}
	}
	return false
}

// KindsFor lists the kinds that may be attached to an owner type (for the upload form).
func KindsFor(ownerType string) []string {
	order := []string{"signed_contract", "amendment", "quotation", "customer_order", "po_signed", "supplier_confirmation", "supplier_invoice", "receipt_note",
		"packing_list", "co", "cq", "delivery_note", "acceptance", "sales_invoice", "bank", "photo", "minutes", "correspondence", "profile", "nda", "other"}
	out := []string{}
	for _, k := range order {
		if allowed(ownerType, k) {
			out = append(out, k)
		}
	}
	return out
}
