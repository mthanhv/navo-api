// Package router wires every route with the permission it requires.
package router

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"navo-api/internal/access"
	"navo-api/internal/auth"
	"navo-api/internal/config"
	"navo-api/internal/core"
	"navo-api/internal/masterdata"
	"navo-api/internal/middleware"
	"navo-api/internal/response"
	"navo-api/internal/users"
)

// New builds the HTTP handler.
func New(cfg config.Config, pool *pgxpool.Pool) http.Handler {
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
		if err := pool.Ping(ctx); err != nil {
			response.Fail(c, err)
			return
		}
		response.OK(c, gin.H{"status": "ready"})
	})

	authSvc := auth.NewService(pool, cfg.SessionTTL, cfg.CookieSecure)
	md := &masterdata.Handlers{Pool: pool}
	co := &core.Handlers{Pool: pool, TZ: cfg.Timezone}
	us := &users.Handlers{Pool: pool}
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

	a.GET("/organizations", need(access.ContractsRead), md.ListOrganizations)
	a.POST("/organizations", need(access.PartnersWrite), md.CreateOrganization)
	a.PATCH("/organizations/:id", need(access.PartnersWrite), md.UpdateOrganization)
	a.GET("/products", need(access.ContractsRead), md.ListProducts)
	a.POST("/products", need(access.CatalogWrite), md.CreateProduct)
	a.PATCH("/products/:id", need(access.CatalogWrite), md.UpdateProduct)

	a.GET("/contracts", need(access.ContractsRead), co.ListContracts)
	a.POST("/contracts", need(access.ContractsWrite), co.CreateContract)
	a.GET("/contracts/:id", need(access.ContractsRead), co.GetContract)
	a.POST("/contracts/:id/activate", need(access.ContractsWrite), co.ActivateContract)
	a.DELETE("/contracts/:id", need(access.ContractsWrite), co.DeleteContract)

	a.POST("/contracts/:id/purchase-orders", need(access.ProcurementWrite), co.CreatePurchaseOrder)
	a.GET("/purchase-orders/:id", need(access.ContractsRead), co.GetPurchaseOrder)
	a.POST("/purchase-orders/:id/confirm", need(access.ProcurementWrite), co.ConfirmPurchaseOrder)
	a.POST("/purchase-orders/:id/cancel", need(access.ProcurementWrite), co.CancelPurchaseOrder)

	a.POST("/purchase-orders/:id/goods-receipts", need(access.FulfillmentWrite), co.CreateGoodsReceipt)
	a.POST("/goods-receipts/:id/confirm", need(access.FulfillmentConfirm), co.ConfirmGoodsReceipt)
	a.POST("/goods-receipts/:id/cancel", need(access.FulfillmentWrite), co.CancelGoodsReceipt)

	a.POST("/contracts/:id/delivery-orders", need(access.FulfillmentWrite), co.CreateDeliveryOrder)
	a.POST("/delivery-orders/:id/confirm", need(access.FulfillmentConfirm), co.ConfirmDeliveryOrder)
	a.POST("/delivery-orders/:id/cancel", need(access.FulfillmentWrite), co.CancelDeliveryOrder)

	a.GET("/contracts/:id/documents", need(access.ContractsRead), co.ContractDocuments)
	a.GET("/purchase-orders", need(access.ContractsRead), co.ListPurchaseOrders)
	a.GET("/delivery-orders", need(access.ContractsRead), co.ListDeliveryOrders)
	a.POST("/purchase-orders/:id/override-request", need(access.ProcurementWrite), co.RequestPOOverride)
	a.POST("/delivery-orders/:id/override-request", need(access.FulfillmentWrite), co.RequestDOOverride)
	a.GET("/override-requests", need(access.QuantityOverride), co.ListOverrides)
	a.POST("/override-requests/:id/approve", need(access.QuantityOverride), co.ApproveOverride)
	a.POST("/override-requests/:id/reject", need(access.QuantityOverride), co.RejectOverride)
	a.GET("/reports/contract-lines", need(access.ContractsRead), co.ReportContractLines)
	a.GET("/reports/overview", need(access.ContractsRead), co.ReportOverview)

	r.NoRoute(func(c *gin.Context) { response.Fail(c, errNotFound) })
	return r
}
