// Package apperrors defines the errors handlers return. Each carries a stable
// code (and field codes or a business reason) that clients word in the user's
// language; the English message is only a fallback.
package apperrors

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Error codes (error_code in the response envelope).
const (
	CodeInvalidRequest  = "validation.invalid_request"
	CodeUnauthorized    = "auth.unauthorized"
	CodeForbidden       = "auth.forbidden"
	CodeNotFound        = "resource.not_found"
	CodeConflict        = "resource.conflict"
	CodeRuleViolation   = "rule.violation"
	CodeTooManyRequests = "rate.limited"
	CodeInternal        = "server.internal"
)

// FieldError names one field (a JSON path such as lines[1].quantity) and what
// is wrong with it.
type FieldError struct {
	Field  string         `json:"field"`
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// FieldErrors collects every problem of one request so they are reported together.
type FieldErrors []FieldError

// Add appends a field problem.
func (f *FieldErrors) Add(field, code string, params map[string]any) {
	*f = append(*f, FieldError{Field: field, Code: code, Params: params})
}

// Err returns an invalid-request error when any problem was collected, else nil.
func (f FieldErrors) Err() error {
	if len(f) == 0 {
		return nil
	}
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "Some fields are invalid.", Fields: f}
}

// Error is the single error type the response layer understands.
type Error struct {
	Status  int
	Code    string
	Message string
	Reason  string
	Params  map[string]any
	Fields  FieldErrors
}

func (e *Error) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s (%s): %s", e.Code, e.Reason, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Invalid reports a single field problem.
func Invalid(field, code string, params map[string]any) error {
	f := FieldErrors{}
	f.Add(field, code, params)
	return f.Err()
}

// NotFound hides whether a record does not exist or is outside the caller's scope.
func NotFound() error {
	return &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: "The record was not found."}
}

// Unauthorized means the session is missing or expired.
func Unauthorized() error {
	return &Error{Status: http.StatusUnauthorized, Code: CodeUnauthorized, Message: "Please sign in again."}
}

// Forbidden means the caller lacks a permission.
func Forbidden(reason string) error {
	return &Error{Status: http.StatusForbidden, Code: CodeForbidden, Reason: reason, Message: "You do not have permission to do this."}
}

// Conflict reports a version clash, duplicate or retry problem.
func Conflict(reason, message string, params map[string]any) error {
	return &Error{Status: http.StatusConflict, Code: CodeConflict, Reason: reason, Message: message, Params: params}
}

// Rule reports a broken business rule the user can act on.
func Rule(reason, message string, params map[string]any) error {
	return &Error{Status: http.StatusConflict, Code: CodeRuleViolation, Reason: reason, Message: message, Params: params}
}

// TooManyRequests reports rate limiting.
func TooManyRequests() error {
	return &Error{Status: http.StatusTooManyRequests, Code: CodeTooManyRequests, Message: "Too many attempts. Please wait and try again."}
}

// From converts any error into an *Error, mapping database constraint
// violations to client errors and everything else to an internal error
// (whose cause is logged, never returned).
func From(err error) *Error {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return NotFound().(*Error)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return &Error{Status: http.StatusConflict, Code: CodeConflict, Reason: "duplicate", Message: "A record with the same value already exists.",
				Params: map[string]any{"constraint": pgErr.ConstraintName}}
		case "23503": // foreign_key_violation
			return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Reason: "invalid_reference", Message: "A referenced record does not exist or does not belong here.",
				Params: map[string]any{"constraint": pgErr.ConstraintName}}
		case "23514", "23502", "22003": // check, not null, numeric out of range
			return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Reason: "constraint", Message: "A value is not allowed.",
				Params: map[string]any{"constraint": pgErr.ConstraintName}}
		case "P0001": // raised by our triggers, e.g. forbidding deletes of confirmed documents
			if strings.HasPrefix(pgErr.Message, "navo:") {
				return &Error{Status: http.StatusConflict, Code: CodeRuleViolation, Reason: strings.TrimPrefix(pgErr.Message, "navo:"), Message: "This action is not allowed for the record's state."}
			}
		}
	}
	return &Error{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "Something went wrong. Please try again."}
}
