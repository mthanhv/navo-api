// Package reqctx carries request-scoped values (request id, signed-in user)
// through context.Context so services and logs can read them without Gin.
package reqctx

import (
	"context"
	"regexp"

	"github.com/google/uuid"
)

type key int

const (
	requestIDKey key = iota
	actorKey
)

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

// NormalizeRequestID keeps a client's request id when it is well formed and
// otherwise generates one.
func NormalizeRequestID(candidate string) string {
	if validRequestID.MatchString(candidate) {
		return candidate
	}
	return "api-" + uuid.NewString()
}

// WithRequestID stores the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request id, or "" outside a request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithActor stores the signed-in user id.
func WithActor(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, actorKey, id)
}

// Actor returns the signed-in user id, or uuid.Nil.
func Actor(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(actorKey).(uuid.UUID)
	return id
}
