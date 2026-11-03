package httpcap

import (
	"fmt"
	"net/netip"
	"syscall"
)

// nonPublic lists the IPv4 ranges, besides the ones netip classifies (loopback, private, link-local,
// multicast, unspecified), that do not lead to the public internet.
//
//nolint:gochecknoglobals // immutable table, built once
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT, also some cloud metadata (100.100.100.200)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, including the broadcast address
	netip.MustParsePrefix("fec0::/10"),       // deprecated site-local
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2001::/32"),       // Teredo: tunnels to an embedded IPv4 address
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
}

// Prefixes of IPv6 addresses that embed an IPv4 address, which is checked in turn.
//
//nolint:gochecknoglobals // immutable, built once
var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")   // well-known NAT64: the low 32 bits
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48") // local-use NAT64: the low 32 bits
	sixToFour  = netip.MustParsePrefix("2002::/16")      // 6to4: bits 16-47
)

// isPrivate reports whether addr is not a public unicast address: loopback, private and unique-local,
// link-local (which includes the 169.254.169.254 cloud metadata address), multicast, unspecified, reserved,
// shared, documentation, or an IPv6 address embedding (mapping, NAT64, 6to4) any of these.
func isPrivate(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || inNonPublic(addr) {
		return true
	}

	embedded, ok := embeddedIPv4(addr)

	return ok && isPrivate(embedded)
}

func inNonPublic(addr netip.Addr) bool {
	for _, prefix := range nonPublic {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// embeddedIPv4 returns the IPv4 address a NAT64 or 6to4 address routes to.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	b := addr.As16()

	switch {
	case nat64.Contains(addr) || nat64Local.Contains(addr):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(addr):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	default:
		return netip.Addr{}, false
	}
}

// refusePrivate is the dialer's Control function when private networks are blocked: it runs for every
// address a connection is about to be made to, after name resolution, so a name that resolves (or is
// rebound) to a private address is refused as well as an IP literal.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable dial address %q", ErrPrivateAddress, address)
	}

	if isPrivate(addrPort.Addr()) {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, addrPort.Addr())
	}

	return nil
}
