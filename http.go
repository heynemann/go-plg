package plg

import (
	"context"
	"log/slog"
	"time"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/httpcap"
)

// HTTPPolicy lets the plugins of one Register call make HTTP requests, which the host makes on their behalf.
// Without [WithHTTP], every request is refused. Zero fields take the documented defaults.
//
// Go plugins use the standard net/http client (http.Get, http.Client, http.NewRequest, ...): inside the guest
// http.DefaultTransport hands each request to the host. TypeScript plugins use fetch and Lua plugins
// http.request{method, url, headers, body}, both reaching the same host transport. Redirects are followed by the
// plugin's side (the Go client, fetch or http.request), each hop being a new request checked like the first.
// Every request, made or refused, is logged with the plugin's key through the container's logger.
type HTTPPolicy struct {
	// BlockPrivateNetworks refuses connections to loopback, private, unique-local, link-local (including the
	// 169.254.169.254 cloud metadata service), carrier-grade NAT, multicast and reserved addresses. It is
	// checked on the address each connection is about to use, after DNS resolution, so an allowed name that
	// resolves (or is rebound) to such an address is refused too. Nil means true; set it to new(false) to
	// reach such hosts, for example a test server on loopback.
	BlockPrivateNetworks *bool
	// AllowedHosts lists the hosts plugins may reach; it is required. An entry is a host name
	// ("api.example.com"; Unicode or punycode, any case, an optional trailing dot), a wildcard matching the
	// strict subdomains of a name ("*.example.com"), or an IP address ("10.1.2.3", "2001:db8::1" or
	// "[2001:db8::1]"), each optionally followed by ":<port>"; without a port, any port is allowed. A URL's
	// port is its explicit one or its scheme's default (80 or 443). Only http and https URLs are allowed.
	AllowedHosts []string
	// Timeout bounds each request, from its start to the end of its response body. It never extends the
	// call's context: a request also ends when the call's context does. Default 10s.
	Timeout time.Duration
	// MaxRequestBytes bounds each request's URL, header and body together. Default 1 MiB, at most 1 GiB.
	MaxRequestBytes int
	// MaxResponseBytes bounds each response body; a larger one fails the request after at most that many
	// bytes were read. Default 4 MiB, at most 1 GiB.
	MaxResponseBytes int
	// MaxRequestsPerCall bounds the requests one plugin call (or Create) makes. Default 16.
	MaxRequestsPerCall int
}

// WithHTTP lets the registered plugins make HTTP requests under policy. Register fails with
// [ErrInvalidHTTPPolicy] if the policy is invalid (no AllowedHosts, a malformed entry, a negative or too
// large limit).
func WithHTTP(policy HTTPPolicy) RegisterOption {
	return func(c *registerConfig) {
		c.http = &policy
	}
}

// httpClient returns the client enforcing the Register call's policy: a refusing one without WithHTTP.
func httpClient(policy *HTTPPolicy, logger *slog.Logger) (*httpcap.Client, error) {
	if policy == nil {
		return httpcap.Disabled(logger), nil
	}

	client, err := httpcap.New(httpcap.Policy{
		AllowedHosts:         policy.AllowedHosts,
		BlockPrivateNetworks: policy.BlockPrivateNetworks == nil || *policy.BlockPrivateNetworks,
		Timeout:              policy.Timeout,
		MaxRequestBytes:      policy.MaxRequestBytes,
		MaxResponseBytes:     policy.MaxResponseBytes,
		MaxRequestsPerCall:   policy.MaxRequestsPerCall,
	}, logger)
	if err != nil {
		return nil, err //nolint:wrapcheck // it wraps ErrInvalidHTTPPolicy (httpcap.ErrInvalidPolicy) with the reason
	}

	return client, nil
}

// httpHandler returns the http_roundtrip handler of the instances of the plugin key in lang, enforcing the
// Register call's policy, its panics reported and its requests counted in meters.
func (c *Container) httpHandler(reg *registration, key string, lang Language, meters *pluginMeters) engine.HTTPFunc {
	return reg.http.Handler(key, httpcap.Hooks{Panicked: c.httpPanicked(key, lang), Done: meters.requested})
}

// httpPanicked returns the function the HTTP capability passes its panics to: they become reported
// PanicErrors in [PhaseHTTP], and the plugin's request fails with an internal error.
func (c *Container) httpPanicked(key string, lang Language) func(context.Context, any) {
	return func(ctx context.Context, value any) {
		c.panics.recovered(ctx, &PanicError{Key: key, Language: lang, Phase: PhaseHTTP}, value)
	}
}
