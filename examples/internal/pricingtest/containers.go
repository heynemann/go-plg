package pricingtest

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
)

// PublishOnFreePort publishes a container's port (like "5432/tcp") on loopback, on a port free on this machine.
// Docker picks a published port where the engine runs; under Docker Desktop that is a VM which cannot see this
// machine's listeners, so a port another local process already holds shadows the container (its readiness check
// then talks to that process until it times out). Asking this machine's kernel for the port avoids it.
func PublishOnFreePort(ctx context.Context, containerPort string) (testcontainers.CustomizeRequestOption, error) {
	loopback := netip.AddrFrom4([4]byte{127, 0, 0, 1})

	var config net.ListenConfig

	listener, err := config.Listen(ctx, "tcp", netip.AddrPortFrom(loopback, 0).String())
	if err == nil {
		port := netip.MustParseAddrPort(listener.Addr().String()).Port()
		err = listener.Close()
		publish := testcontainers.WithHostConfigModifier(func(host *container.HostConfig) {
			host.PortBindings = network.PortMap{
				network.MustParsePort(containerPort): {{HostIP: loopback, HostPort: strconv.Itoa(int(port))}},
			}
		})

		if err == nil {
			return publish, nil
		}
	}

	return nil, fmt.Errorf("finding a free port: %w", err)
}
