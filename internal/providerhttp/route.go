package providerhttp

import (
	"strings"
	"sync"
)

// Route is where one provider's requests go and whether this process
// authenticates them, set by the parent after the client is built. A parent
// that fronts model traffic with a gateway names the gateway as the base URL
// and says the gateway adds the credential, so the process holds no key and a
// request carries no credential header of its own.
//
// It is the process's, not a session's: one process hosts one session, and a
// create that names a route replaces whatever the process held. A Transport
// with no Route sends to its own BaseURL with the key its APIKeyProvider
// supplies.
type Route struct {
	mu           sync.RWMutex
	baseURL      string
	gatewayKeyed bool
}

// Set replaces the route. An empty baseURL keeps the transport's own base URL;
// gatewayKey true sends requests with no credential header and no local key.
func (r *Route) Set(baseURL string, gatewayKey bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.baseURL = strings.TrimRight(baseURL, "/")
	r.gatewayKeyed = gatewayKey
}

// GatewayKey reports whether the gateway supplies this provider's credential.
func (r *Route) GatewayKey() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.gatewayKeyed
}

// base returns the base URL to send to, fallback when the route names none.
func (r *Route) base(fallback string) string {
	if r == nil {
		return fallback
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.baseURL != "" {
		return r.baseURL
	}
	return fallback
}
