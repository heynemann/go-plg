// Package httpcap is the host side of plugins' HTTP capability: it validates a plugin's HTTP policy and makes
// (or refuses) the requests its guests send through plg.http_roundtrip, logging every one by plugin key.
//
// A [Client] belongs to one Register call; its [Client.Handler] serves one plugin. Requests are refused when:
// HTTP is not enabled; the call made more than MaxRequestsPerCall requests; the encoded request exceeds
// MaxRequestBytes; the URL is not http or https; the method is CONNECT; or the URL's host and port are not in
// AllowedHosts. With BlockPrivateNetworks, every address the dialer is about to connect to is checked after
// name resolution, so names resolving (or rebound) to private, loopback, link-local or metadata addresses are
// refused too. Each request ends at Timeout or when the call's context ends, whichever comes first; a response
// body over MaxResponseBytes fails the request after reading at most that much. Redirects are not followed
// here: the guest's http.Client follows them, each hop being a new, checked request.
package httpcap

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Policy defaults, applied to zero fields.
const (
	DefaultTimeout            = 10 * time.Second
	DefaultMaxRequestBytes    = 1 << 20
	DefaultMaxResponseBytes   = 4 << 20
	DefaultMaxRequestsPerCall = 16
)

// MaxBytes bounds MaxRequestBytes and MaxResponseBytes, so that every exchange fits the 32-bit lengths of
// the guest ABI.
const MaxBytes = 1 << 30

// Transport settings.
const (
	maxResponseHeader = 64 << 10 // bounds a response's header, which MaxResponseBytes does not count
	keepAlive         = 30 * time.Second
	maxIdleConns      = 100
	idleConnTimeout   = 90 * time.Second
)

// Errors. Refusals reach the plugin as the round trip's error message.
var (
	// ErrInvalidPolicy reports a policy [New] cannot accept.
	ErrInvalidPolicy = errors.New("plg: invalid HTTP policy")
	// ErrDisabled refuses the requests of a plugin registered without an HTTP policy.
	ErrDisabled = errors.New("plg http: HTTP is not enabled for this plugin (register it with plg.WithHTTP)")
	// ErrTooManyRequests refuses a request over the call's MaxRequestsPerCall.
	ErrTooManyRequests = errors.New("plg http: too many requests in one call")
	// ErrRequestTooLarge refuses a request whose URL, header and body exceed MaxRequestBytes.
	ErrRequestTooLarge = errors.New("plg http: request too large")
	// ErrResponseTooLarge fails a request whose response body exceeds MaxResponseBytes.
	ErrResponseTooLarge = errors.New("plg http: response too large")
	// ErrScheme refuses a URL that is not http or https.
	ErrScheme = errors.New("plg http: unsupported URL scheme")
	// ErrMethod refuses the CONNECT method: plugins get requests, not tunnels.
	ErrMethod = errors.New("plg http: method not allowed")
	// ErrHostNotAllowed refuses a host and port not in AllowedHosts.
	ErrHostNotAllowed = errors.New("plg http: host not allowed")
	// ErrPrivateAddress refuses a connection to a private network address.
	ErrPrivateAddress = errors.New("plg http: private network address refused")
	// ErrTimeout fails a request that ran past Timeout.
	ErrTimeout = errors.New("plg http: request timed out")
	// ErrBadRequest refuses a request the guest encoded wrongly or that net/http rejects.
	ErrBadRequest = errors.New("plg http: invalid request")
	// ErrFailed reports a request that failed in transit (connection, TLS, protocol errors).
	ErrFailed = errors.New("plg http: request failed")
	// ErrInternal reports a panic in the HTTP capability itself; the handler's panicked function receives it.
	ErrInternal = errors.New("plg http: internal error")
)

// Policy is a Register call's HTTP policy. Zero numeric fields take their defaults.
type Policy struct {
	// AllowedHosts lists the hosts plugins may reach (required): host names, *.wildcards and IP addresses,
	// each with an optional port.
	AllowedHosts         []string
	Timeout              time.Duration
	MaxRequestBytes      int
	MaxResponseBytes     int
	MaxRequestsPerCall   int
	BlockPrivateNetworks bool
}

// withDefaults validates p and fills its zero fields.
func (p Policy) withDefaults() (Policy, error) {
	if len(p.AllowedHosts) == 0 {
		return p, fmt.Errorf("%w: AllowedHosts is required", ErrInvalidPolicy)
	}

	switch {
	case p.Timeout < 0:
		return p, fmt.Errorf("%w: negative Timeout %s", ErrInvalidPolicy, p.Timeout)
	case p.MaxRequestBytes < 0 || p.MaxRequestBytes > MaxBytes:
		return p, fmt.Errorf("%w: MaxRequestBytes %d outside 0..%d", ErrInvalidPolicy, p.MaxRequestBytes, MaxBytes)
	case p.MaxResponseBytes < 0 || p.MaxResponseBytes > MaxBytes:
		return p, fmt.Errorf("%w: MaxResponseBytes %d outside 0..%d", ErrInvalidPolicy, p.MaxResponseBytes,
			MaxBytes)
	case p.MaxRequestsPerCall < 0:
		return p, fmt.Errorf("%w: negative MaxRequestsPerCall %d", ErrInvalidPolicy, p.MaxRequestsPerCall)
	}

	p.Timeout = cmp.Or(p.Timeout, DefaultTimeout)
	p.MaxRequestBytes = cmp.Or(p.MaxRequestBytes, DefaultMaxRequestBytes)
	p.MaxResponseBytes = cmp.Or(p.MaxResponseBytes, DefaultMaxResponseBytes)
	p.MaxRequestsPerCall = cmp.Or(p.MaxRequestsPerCall, DefaultMaxRequestsPerCall)

	return p, nil
}

// Client makes the requests of one Register call's plugins under its policy. It is safe for concurrent use.
type Client struct {
	logger    *slog.Logger
	transport http.RoundTripper
	closeIdle func()
	allowed   hosts
	policy    Policy
	enabled   bool
}

// Disabled returns a client that refuses every request (and logs it).
func Disabled(logger *slog.Logger) *Client {
	return &Client{logger: logger, closeIdle: func() {}}
}

// New returns a client enforcing policy, or an error wrapping [ErrInvalidPolicy].
func New(policy Policy, logger *slog.Logger) (*Client, error) {
	policy, err := policy.withDefaults()
	if err != nil {
		return nil, err
	}

	allowed, err := parseHosts(policy.AllowedHosts)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}

	dialer := &net.Dialer{Timeout: policy.Timeout, KeepAlive: keepAlive}
	if policy.BlockPrivateNetworks {
		dialer.Control = refusePrivate
	}

	transport := &http.Transport{
		Proxy:                  nil, // never the environment's proxy: the policy is about the real destination
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           maxIdleConns,
		IdleConnTimeout:        idleConnTimeout,
		TLSHandshakeTimeout:    policy.Timeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxResponseHeader,
	}

	return &Client{
		logger:    logger,
		transport: transport,
		closeIdle: transport.CloseIdleConnections,
		allowed:   allowed,
		policy:    policy,
		enabled:   true,
	}, nil
}

// Close closes the client's idle connections. Requests in flight are unaffected.
func (c *Client) Close() {
	c.closeIdle()
}
