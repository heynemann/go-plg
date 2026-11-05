//go:build unix

package pricingtest

import (
	"net/netip"
	"strconv"
	"syscall"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

// PublishOnFreePort publishes a container's port (like "5432/tcp") on loopback, on a port free on this machine and
// reserved for tb until it ends.
//
// Docker picks a published port where the engine runs; under Docker Desktop that is a VM which cannot see this
// machine's listeners, so a port another local process already holds shadows the container (its readiness check
// then talks to that process until it times out). Asking this machine's kernel for the port avoids it.
//
// The port stays reserved: a socket bound to it (SO_REUSEADDR, never listening) is held until tb's cleanup, so the
// kernel hands it to no other test, in this process or another, while Docker can still listen on it. A port asked
// for and released at once was handed out again to tests starting containers in parallel, whose start then failed
// with "port is already allocated".
func PublishOnFreePort(tb testing.TB, containerPort string) testcontainers.CustomizeRequestOption {
	tb.Helper()

	reservation, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	require.NoError(tb, err, "reserving a port")
	syscall.CloseOnExec(reservation)
	tb.Cleanup(func() { require.NoError(tb, syscall.Close(reservation), "releasing a reserved port") })

	loopback := [4]byte{127, 0, 0, 1}

	err = syscall.SetsockoptInt(reservation, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	require.NoError(tb, err, "reserving a port")
	require.NoError(tb, syscall.Bind(reservation, &syscall.SockaddrInet4{Addr: loopback}), "reserving a port")

	bound, err := syscall.Getsockname(reservation)
	require.NoError(tb, err, "reserving a port")

	addr, ok := bound.(*syscall.SockaddrInet4)
	require.True(tb, ok, "an IPv4 socket has an IPv4 address")

	host := network.PortBinding{HostIP: netip.AddrFrom4(loopback), HostPort: strconv.Itoa(addr.Port)}

	return testcontainers.WithHostConfigModifier(func(config *container.HostConfig) {
		config.PortBindings = network.PortMap{network.MustParsePort(containerPort): {host}}
	})
}
