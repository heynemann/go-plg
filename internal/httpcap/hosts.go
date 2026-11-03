package httpcap

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

var (
	errEmptyHost    = errors.New("empty host")
	errNotHost      = errors.New("a host with an optional port, not a URL")
	errBadPort      = errors.New("invalid port")
	errZone         = errors.New("IPv6 zones are not allowed")
	errWildcardIP   = errors.New("wildcards apply to host names, not IP addresses")
	errBadHostName  = errors.New("invalid host name")
	errMissingClose = errors.New("missing ]")
)

// rule is one AllowedHosts entry, normalized.
type rule struct {
	addr     netip.Addr // an IP rule's address (unmapped); invalid for host name rules
	name     string     // a host name rule's canonical name; a wildcard's is ".<name>"
	port     string     // canonical decimal port, or "" for any port
	wildcard bool       // "*.<name>": strict subdomains of name only
}

// hosts is the parsed AllowedHosts list.
type hosts []rule

// parseHosts parses every entry, failing on the first invalid one.
func parseHosts(entries []string) (hosts, error) {
	parsed := make(hosts, 0, len(entries))

	for _, entry := range entries {
		r, err := parseRule(entry)
		if err != nil {
			return nil, fmt.Errorf("allowed host %q: %w", entry, err)
		}

		parsed = append(parsed, r)
	}

	return parsed, nil
}

// parseRule parses one entry: a host name ("api.example.com", Unicode or punycode, any case, an optional
// trailing dot), a wildcard over a name's subdomains ("*.example.com"), an IPv4 or IPv6 address ("10.0.0.1",
// "::1", "[::1]"), each optionally followed by ":<port>".
func parseRule(entry string) (rule, error) {
	if entry == "" {
		return rule{}, errEmptyHost
	}

	if strings.ContainsAny(entry, "/?#@ \t\r\n") {
		return rule{}, errNotHost
	}

	parts, err := splitEntry(entry)
	if err != nil {
		return rule{}, err
	}

	port, err := entryPort(parts.port)
	if err != nil {
		return rule{}, err
	}

	host, wildcard := strings.CutPrefix(parts.host, "*.")

	addr, ipErr := netip.ParseAddr(host)
	if ipErr == nil {
		return addrRule(addr, port, wildcard)
	}

	name, err := canonicalName(host)
	if err != nil {
		return rule{}, err
	}

	if wildcard {
		name = "." + name // a strict subdomain ends with it and is longer
	}

	return rule{name: name, port: port, wildcard: wildcard}, nil
}

func addrRule(addr netip.Addr, port string, wildcard bool) (rule, error) {
	switch {
	case wildcard:
		return rule{}, errWildcardIP
	case addr.Zone() != "":
		return rule{}, errZone
	default:
		return rule{addr: addr.Unmap(), port: port}, nil
	}
}

// entryPort returns an entry's canonical port, or "" for any port.
func entryPort(port string) (string, error) {
	if port == "" {
		return "", nil
	}

	return canonicalPort(port)
}

// entryParts is an entry's host and port ("" when absent).
type entryParts struct {
	host, port string
}

// splitEntry splits an entry into its host and port. A bare IPv6 address has several colons and no port; a
// bracketed one may have a port.
func splitEntry(entry string) (entryParts, error) {
	if strings.HasPrefix(entry, "[") {
		return splitBracketed(entry)
	}

	if strings.Count(entry, ":") > 1 {
		return entryParts{host: entry}, nil
	}

	host, port, found := strings.Cut(entry, ":")
	if found && port == "" {
		return entryParts{}, errBadPort
	}

	return entryParts{host: host, port: port}, nil
}

// splitBracketed splits "[<IPv6>]" or "[<IPv6>]:<port>".
func splitBracketed(entry string) (entryParts, error) {
	end := strings.IndexByte(entry, ']')
	if end < 0 {
		return entryParts{}, errMissingClose
	}

	host, after := entry[1:end], entry[end+1:]
	if after == "" {
		return entryParts{host: host}, nil
	}

	port, ok := strings.CutPrefix(after, ":")
	if !ok || port == "" {
		return entryParts{}, errBadPort
	}

	return entryParts{host: host, port: port}, nil
}

// maxPort is the largest TCP port.
const maxPort = 65535

// canonicalPort returns port in canonical decimal form ("0443" is "443").
func canonicalPort(port string) (string, error) {
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 || n > maxPort {
		return "", fmt.Errorf("%w %q", errBadPort, port)
	}

	return strconv.FormatUint(n, 10), nil
}

// Host name limits (RFC 1035).
const (
	maxName  = 253
	maxLabel = 63
)

// canonicalName returns a host name in the form a dialer resolves: without its trailing dot, IDNA ASCII
// (punycode) when it is not ASCII already (as net/http converts it), lower case. Names with empty or overlong
// labels, or with characters outside letters, digits, '-' and '_', are not host names.
func canonicalName(host string) (string, error) {
	name := strings.TrimSuffix(host, ".")

	if !isASCII(name) {
		var err error

		name, err = idna.Lookup.ToASCII(name)
		if err != nil {
			return "", fmt.Errorf("%w: %w", errBadHostName, err)
		}
	}

	name = strings.ToLower(name)
	if !validName(name) {
		return "", fmt.Errorf("%w %q", errBadHostName, host)
	}

	return name, nil
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}

	return true
}

// validName reports whether a lower-case ASCII name is made of valid labels.
func validName(name string) bool {
	if name == "" || len(name) > maxName {
		return false
	}

	for label := range strings.SplitSeq(name, ".") {
		if !validLabel(label) {
			return false
		}
	}

	return true
}

// validLabel reports whether a label is 1 to 63 letters, digits, '-' or '_'.
func validLabel(label string) bool {
	if label == "" || len(label) > maxLabel {
		return false
	}

	for i := range len(label) {
		c := label[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}

	return true
}

// allows reports whether the policy allows the URL's host and port. The port is the URL's, or its scheme's
// default.
func (h hosts) allows(target *url.URL) bool {
	port := target.Port()
	if port == "" {
		port = defaultPort(target.Scheme)
	}

	port, err := canonicalPort(port)
	if err != nil {
		return false
	}

	host := target.Hostname()

	// A zoned address ("fe80::1%eth0") never equals a rule's address, which has no zone.
	addr, ipErr := netip.ParseAddr(host)
	if ipErr == nil {
		return h.allowsAddr(addr.Unmap(), port)
	}

	name, err := canonicalName(host)
	if err != nil {
		return false
	}

	return h.allowsName(name, port)
}

func (h hosts) allowsAddr(addr netip.Addr, port string) bool {
	for i := range h {
		if h[i].addr.IsValid() && h[i].addr == addr && (h[i].port == "" || h[i].port == port) {
			return true
		}
	}

	return false
}

func (h hosts) allowsName(name, port string) bool {
	for i := range h {
		r := &h[i]
		if r.name == "" || (r.port != "" && r.port != port) {
			continue
		}

		if name == r.name || (r.wildcard && len(name) > len(r.name) && strings.HasSuffix(name, r.name)) {
			return true
		}
	}

	return false
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}

	return "80"
}

// hostPort is the host and port a URL dials, for logs and error messages.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}

	return net.JoinHostPort(u.Hostname(), port)
}
