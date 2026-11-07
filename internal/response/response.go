// Package response writes the JSON envelope every endpoint uses (same shape
// as Lova): request_id, status, success, data or error_code/error/reason/params/errors.
package response

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"navo-api/internal/apperrors"
	"navo-api/internal/reqctx"
)

// Envelope is the response body.
type Envelope struct {
	RequestID string                `json:"request_id"`
	Status    int                   `json:"status"`
	Success   bool                  `json:"success"`
	Data      any                   `json:"data,omitempty"`
	ErrorCode string                `json:"error_code,omitempty"`
	Error     string                `json:"error,omitempty"`
	Reason    string                `json:"reason,omitempty"`
	Params    map[string]any        `json:"params,omitempty"`
	Errors    apperrors.FieldErrors `json:"errors,omitempty"`
}

// Page is a paginated list.
type Page struct {
	Items      any        `json:"items"`
	Pagination Pagination `json:"pagination"`
}

// Pagination keeps Lova's camelCase keys for client compatibility.
type Pagination struct {
	Page       int `json:"page"`
	PageSize   int `json:"pageSize"`
	Total      int `json:"total"`
	TotalPages int `json:"totalPages"`
}

// OK answers 200 with data.
func OK(c *gin.Context, data any) { JSON(c, http.StatusOK, data) }

// Created answers 201 with data.
func Created(c *gin.Context, data any) { JSON(c, http.StatusCreated, data) }

// JSON answers a success with any status.
func JSON(c *gin.Context, status int, data any) {
	c.JSON(status, Envelope{RequestID: reqctx.RequestID(c.Request.Context()), Status: status, Success: true, Data: data})
}

// List answers a page of items.
func List(c *gin.Context, items any, page, pageSize, total int) {
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	OK(c, Page{Items: items, Pagination: Pagination{Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages}})
}

// Fail answers an error. Server faults are logged with their cause and
// returned without internal detail; the request id lets support find the log.
func Fail(c *gin.Context, err error) {
	appErr := apperrors.From(err)
	ctx := c.Request.Context()
	if appErr.Status >= http.StatusInternalServerError {
		slog.ErrorContext(ctx, "request failed", "request_id", reqctx.RequestID(ctx), "error", err.Error())
	}
	c.AbortWithStatusJSON(appErr.Status, Envelope{
		RequestID: reqctx.RequestID(ctx), Status: appErr.Status, Success: false,
		ErrorCode: appErr.Code, Error: appErr.Message, Reason: appErr.Reason, Params: appErr.Params, Errors: appErr.Fields,
	})
}
