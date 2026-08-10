package httplog

import (
	"context"
	"testing"
)

func TestSessionIDContextRoundTrip(t *testing.T) {
	ctx := WithSessionID(context.Background(), "sess-42")
	if got := SessionIDFromContext(ctx); got != "sess-42" {
		t.Errorf("SessionIDFromContext = %q, want %q", got, "sess-42")
	}
}

func TestSessionIDFromContextEmpty(t *testing.T) {
	if got := SessionIDFromContext(context.Background()); got != "" {
		t.Errorf("SessionIDFromContext = %q, want empty", got)
	}
}
