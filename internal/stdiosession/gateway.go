package stdiosession

import (
	"net/url"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/provider"
)

// gatewayPlan is what a create's `harness.providers` and
// `harness.browser_proxy` ask for, validated and not yet applied. It is
// applied only once the create is accepted, because the routes are the
// process's and a refused create must leave them as they were.
type gatewayPlan struct {
	routes       map[provider.Name]ProviderRoute
	browserProxy *string
}

// planGateway validates the create's gateway fields. A nil plan means the
// create named none.
func (s *Server) planGateway(h *CreateHarness) (*gatewayPlan, *rpcError) {
	if h == nil || (len(h.Providers) == 0 && h.BrowserProxy == nil) {
		return nil, nil
	}
	plan := &gatewayPlan{routes: map[provider.Name]ProviderRoute{}}
	for name, r := range h.Providers {
		p := provider.Name(name)
		if _, ok := s.opts.Routes[p]; !ok {
			return nil, errorf(CodeInvalidParams, "harness.providers.%s: this process routes %s", name, s.routedNames())
		}
		if base := strings.TrimSpace(r.BaseURL); base != "" {
			if !httpURL(base) {
				return nil, errorf(CodeInvalidParams, "harness.providers.%s.base_url must be an http or https URL", name)
			}
			r.BaseURL = base
		}
		plan.routes[p] = r
	}
	if h.BrowserProxy != nil {
		proxy := strings.TrimSpace(*h.BrowserProxy)
		if proxy != "" && !proxyURL(proxy) {
			return nil, errorf(CodeInvalidParams, "harness.browser_proxy must be an http, https or socks5 URL")
		}
		plan.browserProxy = &proxy
	}
	return plan, nil
}

func (s *Server) routedNames() string {
	var names []string
	for _, p := range []provider.Name{provider.DeepSeek, provider.Gemini, provider.Anthropic} {
		if _, ok := s.opts.Routes[p]; ok {
			names = append(names, string(p))
		}
	}
	return strings.Join(names, ", ")
}

func proxyURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "socks5") && u.Host != ""
}

// gatewaySuppliesKey reports whether a create for model would be sent to a
// gateway that adds the provider's credential: the plan's route when it names
// the provider, otherwise the one the process holds.
func (s *Server) gatewaySuppliesKey(model string, plan *gatewayPlan) bool {
	p := providerOf(model)
	if plan != nil {
		if r, ok := plan.routes[p]; ok {
			return r.GatewaySuppliesKey
		}
	}
	return s.opts.Routes[p].GatewayKey()
}

// applyGateway puts the plan into effect. The caller holds s.mu and has
// checked that no run is in flight, so no request is reading a route as it
// changes.
func (s *Server) applyGateway(plan *gatewayPlan) {
	if plan == nil {
		return
	}
	for p, r := range plan.routes {
		s.opts.Routes[p].Set(r.BaseURL, r.GatewaySuppliesKey)
	}
	if plan.browserProxy != nil && s.opts.Runner.BrowserProxy != nil {
		s.opts.Runner.BrowserProxy.Set(*plan.browserProxy)
	}
}
