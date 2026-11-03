package httpcap_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/httpcap"
)

func TestPrivateAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr    string
		private bool
	}{
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"100.100.100.200", true}, // cloud metadata in shared address space
		{"100.64.0.1", true},
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"255.255.255.255", true},
		{"224.0.0.1", true},
		{"192.0.0.8", true},
		{"198.18.0.1", true},
		{"192.0.2.1", true},
		{"::", true},
		{"::1", true},
		{"fe80::1", true},
		{"fc00::1", true},
		{"fd00:ec2::254", true}, // AWS IPv6 metadata
		{"ff02::1", true},
		{"fec0::1", true},
		{"2001:db8::1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
		{"64:ff9b::a9fe:a9fe", true}, // NAT64 of 169.254.169.254
		{"64:ff9b:1::7f00:1", true},  // local NAT64 of 127.0.0.1
		// Local-use NAT64 (RFC 8215) may use any RFC 6052 layout, so the whole /48 is refused: reading its low 32
		// bits (the /96 layout) took these for public addresses (the suffix bits are the address's to choose).
		{"64:ff9b:1:0:c0:a801:800:0", true},    // /64 layout of 192.168.1.8, low 32 bits 8.0.0.0
		{"64:ff9b:1:0:a:0:100:0", true},        // /64 layout of 10.0.0.1, low 32 bits 1.0.0.0
		{"64:ff9b:1:0:a9:fea9:fe00:0", true},   // /64 layout of 169.254.169.254, low 32 bits 254.0.0.0
		{"64:ff9b:1:c0:a8:108:808:808", true},  // /56 layout of 192.168.1.8, low 32 bits 8.8.8.8
		{"64:ff9b:1:c0a8:1:800:808:808", true}, // /48 layout of 192.168.1.8, low 32 bits 8.8.8.8
		{"64:ff9b:1::808:808", true},           // /96 layout of public 8.8.8.8: refused too, the layout is unknown
		{"2002:a9fe:a9fe::1", true},            // 6to4 of 169.254.169.254
		{"2002:c0a8:0101::1", true},            // 6to4 of 192.168.1.1
		{"2001:0:4136:e378::1", true},          // Teredo
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"172.32.0.1", false},
		{"100.128.0.1", false},
		{"93.184.216.34", false},
		{"::ffff:8.8.8.8", false},
		{"64:ff9b::808:808", false}, // NAT64 of 8.8.8.8
		{"2002:0808:0808::1", false},
		{"2606:4700:4700::1111", false},
	}

	for _, tc := range tests {
		addr := netip.MustParseAddr(tc.addr)
		assert.Equal(t, tc.private, httpcap.PrivateAddr(addr), tc.addr)

		err := httpcap.DialControl(netip.AddrPortFrom(addr, 443).String())
		if tc.private {
			require.ErrorIs(t, err, httpcap.ErrPrivateAddress, tc.addr)
		} else {
			require.NoError(t, err, tc.addr)
		}
	}

	assert.True(t, httpcap.PrivateAddr(netip.Addr{}), "an invalid address is never dialed")
	require.ErrorIs(t, httpcap.DialControl("not an address"), httpcap.ErrPrivateAddress)
}
