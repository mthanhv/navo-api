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

	a.GET("/users", need(access.UsersManage), us.ListUsers)
	a.POST("/users", need(access.UsersManage), us.CreateUser)

	a.GET("/organizations", need(access.ContractsRead), md.ListOrganizations)
	a.POST("/organizations", need(access.PartnersWrite), md.CreateOrganization)
	a.GET("/products", need(access.ContractsRead), md.ListProducts)
	a.POST("/products", need(access.CatalogWrite), md.CreateProduct)

	a.GET("/contracts", need(access.ContractsRead), co.ListContracts)
	a.POST("/contracts", need(access.ContractsWrite), co.CreateContract)
	a.GET("/contracts/:id", need(access.ContractsRead), co.GetContract)
	a.POST("/contracts/:id/activate", need(access.ContractsWrite), co.ActivateContract)

	a.POST("/contracts/:id/purchase-orders", need(access.ProcurementWrite), co.CreatePurchaseOrder)
	a.GET("/purchase-orders/:id", need(access.ContractsRead), co.GetPurchaseOrder)
	a.POST("/purchase-orders/:id/confirm", need(access.ProcurementWrite), co.ConfirmPurchaseOrder)

	a.POST("/purchase-orders/:id/goods-receipts", need(access.FulfillmentWrite), co.CreateGoodsReceipt)
	a.POST("/goods-receipts/:id/confirm", need(access.FulfillmentConfirm), co.ConfirmGoodsReceipt)

	a.POST("/contracts/:id/delivery-orders", need(access.FulfillmentWrite), co.CreateDeliveryOrder)
	a.POST("/delivery-orders/:id/confirm", need(access.FulfillmentConfirm), co.ConfirmDeliveryOrder)

	a.GET("/contracts/:id/documents", need(access.ContractsRead), co.ContractDocuments)
	a.GET("/reports/contract-lines", need(access.ContractsRead), co.ReportContractLines)
	a.GET("/reports/overview", need(access.ContractsRead), co.ReportOverview)

	r.NoRoute(func(c *gin.Context) { response.Fail(c, errNotFound) })
	return r
}
