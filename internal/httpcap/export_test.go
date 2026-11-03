package httpcap

import (
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
)

// Allows parses entries as AllowedHosts and reports whether they allow rawURL.
func Allows(entries []string, rawURL string) (bool, error) {
	parsed, err := parseHosts(entries)
	if err != nil {
		return false, err
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return false, err //nolint:wrapcheck // test helper
	}

	return parsed.allows(u), nil
}

// PrivateAddr exposes isPrivate.
func PrivateAddr(addr netip.Addr) bool { return isPrivate(addr) }

// DialControl runs the dialer's Control function that blocks private networks.
func DialControl(address string) error { return refusePrivate("tcp", address, syscall.RawConn(nil)) }

// EffectivePolicy returns the policy a client enforces, defaults applied.
func EffectivePolicy(c *Client) Policy { return c.policy }

// SetTransport replaces the client's transport.
func SetTransport(c *Client, rt http.RoundTripper) { c.transport = rt }
