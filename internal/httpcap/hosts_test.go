package httpcap_test

import (
	"net/netip"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/httpcap"
)

func TestAllowedHostsMatching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry string
		url   string
		want  bool
	}{
		{"exact name", "api.example.com", "http://api.example.com/x", true},
		{"no port means any port", "api.example.com", "https://api.example.com:8443/", true},
		{"case of the entry", "API.Example.COM", "http://api.example.com/", true},
		{"case of the URL", "api.example.com", "http://api.EXAMPLE.com/", true},
		{"trailing dot in the entry", "api.example.com.", "http://api.example.com/", true},
		{"trailing dot in the URL", "api.example.com", "http://api.example.com./", true},
		{"two trailing dots", "api.example.com", "http://api.example.com../", false},
		{"a subdomain of an exact name", "example.com", "http://api.example.com/", false},
		{"a suffix that is not a subdomain", "example.com", "http://evilexample.com/", false},
		{"the name as a prefix", "example.com", "http://example.com.evil.net/", false},
		{"userinfo is not the host", "api.example.com", "http://api.example.com@evil.net/", false},
		{"fragment is not the host", "api.example.com", "http://evil.net#@api.example.com", false},
		{"underscores", "my_svc.internal", "http://my_svc.internal/", true},
		{"wildcard subdomain", "*.example.com", "http://api.example.com/", true},
		{"wildcard nested subdomain", "*.example.com", "http://a.b.example.com/", true},
		{"wildcard excludes the apex", "*.example.com", "http://example.com/", false},
		{"wildcard is label-aligned", "*.example.com", "http://evil-example.com/", false},
		{"wildcard with port", "*.example.com:8443", "https://a.example.com:8443/", true},
		{"wildcard with another port", "*.example.com:8443", "https://a.example.com/", false},
		{"explicit port", "api.example.com:8443", "https://api.example.com:8443/", true},
		{"explicit port, default used", "api.example.com:8443", "https://api.example.com/", false},
		{"port with leading zeros", "api.example.com:8443", "https://api.example.com:08443/", true},
		{"https default port", "api.example.com:443", "https://api.example.com/", true},
		{"http default port", "api.example.com:443", "http://api.example.com/", false},
		{"http explicit default", "api.example.com:80", "http://api.example.com:80/", true},
		{"URL port out of range", "api.example.com", "http://api.example.com:99999/", false},
		{"URL port zero", "api.example.com", "http://api.example.com:0/", false},
		{"Unicode entry, punycode URL", "bücher.example", "http://xn--bcher-kva.example/", true},
		{"punycode entry, Unicode URL", "xn--bcher-kva.example", "http://bücher.example/", true},
		{"Unicode case folding", "BÜCHER.example", "http://bücher.example/", true},
		{"invalid Unicode in the URL", "bücher.example", "http://b\u00a0cher.example/", false},
		{"IPv4", "127.0.0.1", "http://127.0.0.1:8080/", true},
		{"IPv4 with zeros is not the address", "127.0.0.1", "http://127.000.000.001/", false},
		{"IPv4 as an integer is not the address", "127.0.0.1", "http://2130706433/", false},
		{"IPv4-mapped IPv6 is the IPv4 address", "127.0.0.1", "http://[::ffff:127.0.0.1]/", true},
		{"IPv6", "::1", "http://[::1]:80/", true},
		{"IPv6 bracketed, other spelling", "[::1]", "http://[0:0::1]/", true},
		{"IPv6 with port", "[::1]:8080", "http://[::1]:8080/", true},
		{"IPv6 with another port", "[::1]:8080", "http://[::1]/", false},
		{"IPv6 zone in the URL", "fe80::1", "http://[fe80::1%25eth0]/", false},
		{"an address does not allow names", "127.0.0.1", "http://localhost/", false},
		{"a name does not allow addresses", "localhost", "http://127.0.0.1/", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := httpcap.Allows([]string{"other.example", tc.entry}, tc.url)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got, "%q allows %q", tc.entry, tc.url)
		})
	}
}

func TestInvalidAllowedHosts(t *testing.T) {
	t.Parallel()

	for _, entry := range []string{
		"", "http://a.example", "a.example/path", "a.example?q", "user@a.example", "a b.example", "a.example:",
		"a.example:0", "a.example:65536", "a.example:x", "a.example:-1", "[::1", "[::1]x", "[::1]:", "*.1.2.3.4",
		"fe80::1%eth0", "*", "*.", "a..example", ".", "bad!char.example", strings.Repeat("a", 64) + ".example",
		strings.Repeat("a.", 127) + "aa",
	} {
		_, err := httpcap.Allows([]string{"ok.example", entry}, "http://ok.example/")
		require.Error(t, err, "%q", entry)
		assert.Contains(t, err.Error(), "allowed host", "%q: the error names the entry", entry)
	}
}

// FuzzHostMatching checks the matcher's invariants on arbitrary entries and hosts: it never panics, an entry
// allows its own host, matching ignores ASCII case and one trailing dot, and a wildcard allows subdomains but
// not the apex.
func FuzzHostMatching(f *testing.F) {
	for _, seed := range [][2]string{
		{"api.example.com", "api.example.com"},
		{"*.example.com", "a.example.com"},
		{"127.0.0.1", "127.0.0.1"},
		{"[::1]:8080", "[::1]:8080"},
		{"bücher.example", "xn--bcher-kva.example"},
		{"A.example.", "a.EXAMPLE"},
		{"example.com:443", "example.com"},
		{"my_svc", "MY_SVC."},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, entry, host string) {
		allows := func(h string) bool {
			got, err := httpcap.Allows([]string{entry}, "http://"+h+"/")

			return err == nil && got
		}

		got := allows(host)

		_, entryErr := httpcap.Allows([]string{entry}, "http://x/")
		if entryErr != nil {
			assert.False(t, got, "an invalid entry allows nothing")

			return
		}

		checkSelfMatch(t, entry, allows)

		if got {
			checkSpellings(t, entry, host, allows)
		}

		checkWildcard(t, entry, allows)
	})
}

// checkSelfMatch: a plain host name entry allows its own host.
func checkSelfMatch(t *testing.T, entry string, allows func(string) bool) {
	t.Helper()

	if entry != "" && !strings.ContainsAny(entry, ":[]*") && utf8.ValidString(entry) {
		assert.True(t, allows(entry), "%q allows itself", entry)
	}
}

// checkSpellings: an allowed host is allowed in any ASCII case and, for names, with one trailing dot.
func checkSpellings(t *testing.T, entry, host string, allows func(string) bool) {
	t.Helper()

	if isASCII(host) && !strings.ContainsAny(host, "[]%") {
		assert.True(t, allows(strings.ToUpper(host)), "%q: case-insensitive for %q", entry, host)
		assert.True(t, allows(strings.ToLower(host)), "%q: case-insensitive for %q", entry, host)
	}

	if !strings.ContainsAny(host, ":[]") && !strings.HasSuffix(host, ".") && !isIP(host) {
		assert.True(t, allows(host+"."), "%q: one trailing dot for %q", entry, host)
	}
}

// checkWildcard: "*.name" allows a subdomain of name but not name itself.
func checkWildcard(t *testing.T, entry string, allows func(string) bool) {
	t.Helper()

	apex, ok := strings.CutPrefix(entry, "*.")
	if ok && !strings.Contains(apex, ":") {
		assert.False(t, allows(apex), "%q excludes its apex", entry)
		assert.True(t, allows("sub."+strings.TrimSuffix(apex, ".")), "%q allows a subdomain", entry)
	}
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}

	return true
}

func isIP(s string) bool {
	_, err := netip.ParseAddr(s)

	return err == nil
}
