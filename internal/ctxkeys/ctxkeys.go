// Package ctxkeys defines typed context keys used across the cart service.
// Using a package-scoped unexported type prevents collisions with other packages.
package ctxkeys

import "context"

type key string

const remoteIPKey key = "remote_ip"

// GetRemoteIP returns the client IP stored in ctx, or "" if not set.
func GetRemoteIP(ctx context.Context) string {
	v, _ := ctx.Value(remoteIPKey).(string)
	return v
}

// WithRemoteIP returns a new context with the client IP stored.
func WithRemoteIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, remoteIPKey, ip)
}
