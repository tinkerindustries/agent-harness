package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/agentmeta"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// The control token (docs/RUN-CONTROL.md "Authentication"): resolving the
// operator name a browser start stamps into parent_agent_id, minting a
// fresh request id when the browser has no idempotency key to offer,
// enforcing the bearer token the run-control endpoints require, serving it
// back to a same-machine caller, and the loopback check that decides who
// counts as one.

// operatorName resolves identity.operator for stamping into parent_agent_id
// on a browser start. There is no login system (docs/RUN-CONTROL.md
// "Authentication"): this is a label the operator configures once, read
// server-side so a request header — free text the browser asserts — cannot
// name it. A server with no settings resolver, a read error, a value that
// trims to empty, or one that fails ValidateParentAgentID all resolve to "":
// an unconfigured or malformed operator name degrades to an unnamed person,
// never a failed start (D7).
func (s *Server) operatorName(ctx context.Context) string {
	if s.Settings == nil {
		return ""
	}
	name, err := s.Settings.String(ctx, settings.KeyIdentityOperator)
	if err != nil {
		return ""
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if err := agentmeta.ValidateParentAgentID(name); err != nil {
		return ""
	}
	return name
}

// randomRequestID generates a work request's idempotency key for a browser
// start — the browser form has no idempotency key to offer, and one is
// generated here exactly as the other producers generate theirs
// (docs/RUN-CONTROL.md "POST /api/runs"). The "web-" prefix makes the
// browser's origin obvious in logs and transcripts alongside publish's
// "req-" and deepseek_agent's "mcp-".
func randomRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("httpapi: crypto/rand unavailable: " + err.Error())
	}
	return "web-" + hex.EncodeToString(b[:])
}

// requireControlToken enforces the bearer token the run-control endpoints
// carry (docs/RUN-CONTROL.md "Authentication"). The token is the process's
// own copy of http.control_token, generated and stored at startup by serve
// when the setting is empty; a Server built without that generation (every
// test that does not set one) has an empty token and answers 503 — a missing
// credential must fail closed, never let the request through.
//
// What the token buys and does not buy, plainly: nothing against another
// process running as the same user, which can read the settings table;
// everything on the day the port is exposed off loopback — by a
// `-addr 0.0.0.0`, or by a container port publish — the case
// docs/DESIGN.md §4.2 names as needing authentication beyond this token. The
// comparison is constant time, so a timing side channel cannot probe the
// token byte by byte.
func (s *Server) requireControlToken(w http.ResponseWriter, r *http.Request) bool {
	if s.ControlToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "run control is not configured: no control token has been generated (start harness serve once, or set http.control_token)",
		})
		return false
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "missing bearer token: run-control endpoints require Authorization: Bearer <token>",
		})
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.ControlToken)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
		return false
	}
	return true
}

// handleGetControlToken serves GET /api/control-token: the run-control bearer
// token, to a local caller only (docs/RUN-CONTROL.md "Authentication"). This
// is how the browser and a same-host MCP server get the token; a
// caller that is not on this machine has to be given it out of band, which is
// the property that makes the token worth having. A non-local caller is 403
// with no hint about whether a token exists at all.
func (s *Server) handleGetControlToken(w http.ResponseWriter, r *http.Request) {
	if !isLocalCallerAddr(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "the control token is only served to local callers"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": s.ControlToken})
}

// isLocalCallerAddr reports whether remoteAddr (an "ip:port" string from
// http.Request.RemoteAddr) belongs to a caller close enough to this machine
// to trust with the control token: loopback (127.0.0.0/8, ::1, the name
// "localhost") or a private address (RFC 1918 / RFC 4193, via IP.IsPrivate).
// The private-range allowance exists because the harness runs behind
// docker-compose's published ports (docker-compose.prod.yml): a browser on
// the host hitting 127.0.0.1:8180 arrives inside the container NAT'd through
// the compose network's gateway, not as 127.0.0.1, so a loopback-only check
// rejects genuinely local traffic. The real boundary is still that gateway's
// port publish being loopback-only on the host — nothing outside this machine
// can reach the published port to begin with — so trusting the private range
// on top of it does not admit a caller that could not already reach here.
func isLocalCallerAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
