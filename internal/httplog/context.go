package httplog

import "context"

// sessionIDKey is an unexported type so a session id never collides with a
// value another package stores in the same context.
type sessionIDKey struct{}

// WithSessionID returns a context carrying sessionID for http log capture.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext returns the session id carried by ctx, or "" when
// the context carries none. The transport treats "" as the "harness"
// session.
func SessionIDFromContext(ctx context.Context) string {
	s, _ := ctx.Value(sessionIDKey{}).(string)
	return s
}
