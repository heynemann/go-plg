//go:build unix

// Package containertest starts the containers the storage examples' tests and the rewards platform's end-to-end
// tests (e2e/) run against, each owned by the test that starts it (RULES.md §7).
package containertest

import (
	"crypto/rand"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/minio"
)

// MinIOImage is MinIO built from its release sources (the project no longer publishes images of its own).
const MinIOImage = "alpine/minio:RELEASE.2025-10-15T17-29-55Z"

// StartMinIO starts a MinIO server for tb and its subtests and returns a client reaching it; tb's cleanup
// terminates it, whether the test passes, fails or the server never became ready (RULES.md §7).
func StartMinIO(tb testing.TB) *s3.Client {
	tb.Helper()

	publish := PublishOnFreePort(tb, "9000/tcp")

	// The image runs MinIO as an unprivileged user that cannot write its /data volume.
	ctr, err := minio.Run(tb.Context(), MinIOImage, testcontainers.WithCmd("server", "/tmp/data"), publish)
	testcontainers.CleanupContainer(tb, ctr)
	require.NoError(tb, err)

	endpoint, err := ctr.ConnectionString(tb.Context())
	require.NoError(tb, err)

	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String("http://" + endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(ctr.Username, ctr.Password, ""),
	})
}

// NewBucket creates an empty bucket, of a random name, for one test.
func NewBucket(tb testing.TB, client *s3.Client) string {
	tb.Helper()

	bucket := "plg-" + strings.ToLower(rand.Text())

	_, err := client.CreateBucket(tb.Context(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(tb, err)

	return bucket
}

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
