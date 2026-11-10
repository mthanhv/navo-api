// Package router wires every route with the permission it requires.
package router

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"navo-api/db/migrations"
	"navo-api/internal/access"
	"navo-api/internal/ai"
	"navo-api/internal/auth"
	"navo-api/internal/config"
	"navo-api/internal/core"
	"navo-api/internal/crm"
	"navo-api/internal/db"
	"navo-api/internal/documents"
	"navo-api/internal/files"
	"navo-api/internal/finance"
	"navo-api/internal/mailer"
	"navo-api/internal/masterdata"
	"navo-api/internal/middleware"
	"navo-api/internal/misa"
	"navo-api/internal/response"
	"navo-api/internal/users"
)

// New builds the HTTP handler, sending email through the configured SMTP account.
func New(cfg config.Config, pool *pgxpool.Pool) http.Handler {
	smtp, err := mailer.New(cfg.SMTP)
	if err != nil { // config.Load already refused this; a hand-built Config just runs with email off
		slog.Warn("email is off", "error", err)
	}
	return NewWithMailer(cfg, pool, smtp)
}

// NewWithMailer is New with the email sender given (tests pass a recorder).
func NewWithMailer(cfg config.Config, pool *pgxpool.Pool, mail mailer.Sender) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// Only the edge proxy (Caddy on the private Docker network, or localhost) may set
	// X-Forwarded-For; otherwise a client could pick its own IP and dodge the login limit.
	if err := r.SetTrustedProxies([]string{"127.0.0.1", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}); err != nil {
		panic(err)
	}
	r.Use(middleware.Recover(), middleware.RequestID(), middleware.AccessLog(), middleware.CORS(cfg.AllowedOrigins))

	r.GET("/healthz", func(c *gin.Context) { response.OK(c, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ready(ctx, pool, migrations.FS); err != nil {
			response.Fail(c, err)
			return
		}
		response.OK(c, gin.H{"status": "ready"})
	})

	authSvc := auth.NewService(pool, cfg.SessionTTL, cfg.CookieSecure)
	md := &masterdata.Handlers{Pool: pool}
	co := &core.Handlers{Pool: pool, TZ: cfg.Timezone}
	us := &users.Handlers{Pool: pool}
	docs := &documents.Handlers{Pool: pool, Store: files.Store{Dir: cfg.FilesDir}}
	fin := &finance.Handlers{Pool: pool, TZ: cfg.Timezone}
	need := auth.Require

	v1 := r.Group("/api/v1")
	v1.POST("/auth/login", authSvc.Login)

	a := v1.Group("", authSvc.Authenticate())
	a.POST("/auth/logout", authSvc.Logout)
	a.GET("/me", authSvc.Me)
	a.POST("/me/password", authSvc.ChangePassword)

	a.GET("/users", need(access.UsersManage), us.ListUsers)
	a.POST("/users", need(access.UsersManage), us.CreateUser)
	a.PATCH("/users/:id", need(access.UsersManage), us.UpdateUser)
	a.POST("/users/:id/password", need(access.UsersManage), us.ResetPassword)
	a.GET("/handovers/preview", need(access.UsersManage), us.HandoverPreview)
	a.POST("/handovers", need(access.UsersManage), us.Handover)

	a.GET("/organizations", need(access.ContractsRead), md.ListOrganizations)
	a.POST("/organizations", need(access.PartnersWrite), md.CreateOrganization)
	a.PATCH("/organizations/:id", need(access.PartnersWrite), md.UpdateOrganization)
	a.GET("/products", need(access.ContractsRead), md.ListProducts)
	a.POST("/products", need(access.CatalogWrite), md.CreateProduct)
	a.PATCH("/products/:id", need(access.CatalogWrite), md.UpdateProduct)

	a.GET("/contracts", need(access.ContractsRead), co.ListContracts)
	a.GET("/contracts/owners", need(access.ContractsRead), co.ListOwners)
	a.GET("/search", need(access.ContractsRead), co.Search)
	a.POST("/contracts", need(access.ContractsWrite), co.CreateContract)
	a.GET("/contracts/:id", need(access.ContractsRead), co.GetContract)
	a.GET("/contracts/:id/draft", need(access.ContractsWrite), co.DraftContractInput)
	a.GET("/contracts/:id/history", need(access.ContractsRead), co.ContractHistory)
	a.PATCH("/contracts/:id", need(access.ContractsWrite), co.UpdateDraftContract)
	a.POST("/contracts/:id/activate", need(access.ContractsWrite), co.ActivateContract)
	a.DELETE("/contracts/:id", need(access.ContractsWrite), co.DeleteContract)

	a.POST("/contracts/:id/purchase-orders", need(access.ProcurementWrite), co.CreatePurchaseOrder)
	a.GET("/purchase-orders/:id", need(access.ContractsRead), co.GetPurchaseOrder)
	a.POST("/purchase-orders/:id/confirm", need(access.ProcurementWrite), co.ConfirmPurchaseOrder)
	a.POST("/purchase-orders/:id/cancel", need(access.ProcurementWrite), co.CancelPurchaseOrder)
	a.POST("/purchase-orders/:id/close", need(access.ProcurementWrite), co.ClosePurchaseOrder)

	a.POST("/purchase-orders/:id/goods-receipts", need(access.FulfillmentWrite), co.CreateGoodsReceipt)
	a.POST("/goods-receipts/:id/confirm", need(access.FulfillmentConfirm), co.ConfirmGoodsReceipt)
	a.POST("/goods-receipts/:id/cancel", need(access.FulfillmentWrite), co.CancelGoodsReceipt)
	a.POST("/goods-receipts/:id/reverse", need(access.FulfillmentConfirm), co.ReverseFulfillment("goods_receipt"))

	a.POST("/contracts/:id/delivery-orders", need(access.FulfillmentWrite), co.CreateDeliveryOrder)
	a.POST("/delivery-orders/:id/confirm", need(access.FulfillmentConfirm), co.ConfirmDeliveryOrder)
	a.POST("/delivery-orders/:id/cancel", need(access.FulfillmentWrite), co.CancelDeliveryOrder)
	a.POST("/delivery-orders/:id/reverse", need(access.FulfillmentConfirm), co.ReverseFulfillment("delivery_order"))

	a.GET("/contracts/:id/documents", need(access.ContractsRead), co.ContractDocuments)
	a.GET("/purchase-orders", need(access.ContractsRead), co.ListPurchaseOrders)
	a.GET("/delivery-orders", need(access.ContractsRead), co.ListDeliveryOrders)
	a.POST("/purchase-orders/:id/override-request", need(access.ProcurementWrite), co.RequestPOOverride)
	a.POST("/delivery-orders/:id/override-request", need(access.FulfillmentWrite), co.RequestDOOverride)
	a.GET("/override-requests", need(access.QuantityOverride), co.ListOverrides)
	a.POST("/override-requests/:id/approve", need(access.QuantityOverride), co.ApproveOverride)
	a.POST("/override-requests/:id/reject", need(access.QuantityOverride), co.RejectOverride)
	// Documents (REQUIREMENTS 5.12)
	a.GET("/documents/tree", need(access.ContractsRead), docs.Tree)
	a.GET("/documents/customers/:id", need(access.ContractsRead), docs.CustomerFiles)
	a.GET("/documents/contracts/:id", need(access.ContractsRead), docs.Contract)
	a.GET("/documents/contracts/:id/zip", need(access.ContractsRead), docs.Zip)
	a.POST("/files", need(access.ContractsRead), docs.Upload)
	a.GET("/files/:id", need(access.ContractsRead), docs.Download)
	a.DELETE("/attachment-links/:id", need(access.ContractsRead), docs.Unlink)

	// Finance (N-07): payment terms are part of the contract; money needs finance access.
	a.GET("/contracts/:id/milestones", need(access.ContractsRead), fin.ContractMilestones)
	a.POST("/contracts/:id/milestones", need(access.ContractsRead), fin.CreateMilestone)
	a.PATCH("/milestones/:id", need(access.ContractsRead), fin.UpdateMilestone)
	a.DELETE("/milestones/:id", need(access.ContractsRead), fin.DeleteMilestone)
	a.POST("/milestones/:id/trigger", need(access.ContractsRead), fin.TriggerMilestone)
	a.POST("/milestones/:id/reset", need(access.ContractsRead), fin.ResetMilestone)
	a.GET("/receivables", need(access.FinanceRead), fin.Receivables)
	a.GET("/receipts", need(access.FinanceRead), fin.ListReceipts)
	a.POST("/receipts", need(access.FinanceWrite), fin.CreateReceipt)
	a.POST("/receipts/:id/reverse", need(access.FinanceWrite), fin.ReverseCash("receipt"))
	a.GET("/receipts/:id", need(access.FinanceRead), fin.GetReceipt)
	a.POST("/receipts/:id/allocations", need(access.FinanceWrite), fin.Allocate)
	a.POST("/allocations/:id/reverse", need(access.FinanceWrite), fin.ReverseAllocation)
	a.GET("/invoices", need(access.FinanceRead), fin.ListInvoices)
	a.POST("/contracts/:id/sales-invoices", need(access.FinanceWrite), fin.CreateSalesInvoice)
	a.POST("/purchase-invoices", need(access.FinanceWrite), need(access.PurchaseCostRead), fin.CreatePurchaseInvoice)
	a.POST("/invoices/:kind/:id/cancel", need(access.FinanceWrite), fin.CancelInvoice)
	a.GET("/payments", need(access.FinanceRead), fin.ListPayments)
	a.POST("/payments", need(access.FinanceWrite), fin.CreatePayment)
	a.POST("/payments/:id/reverse", need(access.FinanceWrite), fin.ReverseCash("payment"))
	a.GET("/payments/:id/allocations", need(access.FinanceRead), need(access.PurchaseCostRead), fin.PaymentAllocations)
	a.POST("/payments/:id/allocations", need(access.FinanceWrite), need(access.PurchaseCostRead), fin.AllocatePayment)
	a.POST("/payment-allocations/:id/reverse", need(access.FinanceWrite), fin.ReversePaymentAllocation)

	// AI drafts (5.10, O-17): read a customer's file into a contract draft to check.
	aiH := &ai.Handlers{Pool: pool, AI: ai.New(cfg.AnthropicAPIKey, cfg.AIModel), TZ: cfg.Timezone}
	a.GET("/ai/status", need(access.ContractsRead), aiH.Status)
	a.POST("/ai/contract-draft", need(access.ContractsWrite), aiH.ContractDraft)
	a.POST("/ai/note-parse", need(access.ContractsRead), aiH.NoteParse)

	// Field notes and follow-up tasks (5.10).
	crmH := &crm.Handlers{Pool: pool, Mail: mail}
	a.POST("/notes", need(access.ContractsWrite), crmH.CreateNote)
	a.GET("/notes", need(access.ContractsRead), crmH.ListNotes)
	a.GET("/tasks", crmH.MyTasks)
	a.POST("/tasks", need(access.ContractsWrite), crmH.CreateTask)
	a.POST("/tasks/:id/done", need(access.ContractsWrite), crmH.SetTaskDone(true))
	a.POST("/tasks/:id/reopen", need(access.ContractsWrite), crmH.SetTaskDone(false))

	// People at customers and suppliers, and product-news emails to customer contacts.
	a.GET("/organizations/:id/contacts", need(access.ContractsRead), crmH.ListContacts)
	a.POST("/organizations/:id/contacts", need(access.ContactsWrite), crmH.CreateContact)
	a.PATCH("/contacts/:id", need(access.ContactsWrite), crmH.UpdateContact)
	a.DELETE("/contacts/:id", need(access.ContactsWrite), crmH.DeleteContact)
	a.GET("/contacts/emailable", need(access.ContactsWrite), crmH.ListEmailContacts)
	a.GET("/mailings/status", need(access.ContractsRead), crmH.MailStatus)
	a.GET("/mailings", need(access.ContractsRead), crmH.ListMailings)
	a.GET("/mailings/:id", need(access.ContractsRead), crmH.GetMailing)
	a.POST("/mailings/preview", need(access.ContactsWrite), crmH.PreviewMailing)
	a.POST("/mailings", need(access.ContactsWrite), crmH.CreateMailing)
	crmH.CloseInterrupted(context.Background())

	// MISA SME exchange (5.11): accounting only.
	ms := &misa.Handlers{Pool: pool}
	a.GET("/misa/preview", need(access.FinanceWrite), ms.Preview)
	a.POST("/misa/exports", need(access.FinanceWrite), ms.CreateExport)
	a.GET("/misa/exports/:id/file", need(access.FinanceWrite), ms.File)
	a.PUT("/misa/codes", need(access.FinanceWrite), ms.SetCode)
	a.POST("/misa/import", need(access.FinanceWrite), ms.Import)

	a.GET("/reports/contract-lines", need(access.ContractsRead), co.ReportContractLines)
	a.GET("/reports/overview", need(access.ContractsRead), co.ReportOverview)

	r.NoRoute(func(c *gin.Context) { response.Fail(c, errNotFound) })
	return r
}
