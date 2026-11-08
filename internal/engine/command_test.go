package engine_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/engine"
)

// command compiles a fake WASI command whose _start runs body; it imports fd_write (function 0) and proc_exit
// (function 1), and its memory holds an iovec at 0 for the 5 bytes "ready" at 16.
func command(t *testing.T, body []byte) (*engine.Runtime, *engine.Module) {
	t.Helper()

	data := make([]byte, 16, 21)
	binary.LittleEndian.PutUint32(data[0:], 16)
	binary.LittleEndian.PutUint32(data[4:], 5)

	data = append(data, "ready"...)

	rt := newRuntime(t, newEngine(t, engine.Options{CacheDir: t.TempDir()}))

	mod, err := rt.Compile(t.Context(), "fake", fakeGuest{
		imports: []string{"fd_write", "proc_exit"}, bodies: map[string][]byte{"_start": body}, data: data,
	}.wasm())
	require.NoError(t, err)

	return rt, mod
}

// writeReady is fd_write(stdout, the iovec at 0, 1 iovec, the count at 8), its errno dropped.
func writeReady() []byte {
	return slices.Concat(i32Const(1), i32Const(0), i32Const(1), i32Const(8), call(0), drop())
}

// signal is a writer that closes written on its first write.
type signal struct {
	written chan struct{}
	got     []byte
}

func (s *signal) Write(p []byte) (int, error) {
	if len(s.got) == 0 {
		close(s.written)
	}

	s.got = append(s.got, p...)

	return len(p), nil
}

// TestRunReturnsTheExitCode runs commands that exit with a code, return from _start, write their output and
// trap.
func TestRunReturnsTheExitCode(t *testing.T) {
	t.Parallel()

	_, mod := command(t, slices.Concat(i32Const(3), call(1)))
	code, err := mod.Run(t.Context(), &engine.RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, uint32(3), code)

	_, mod = command(t, slices.Concat(i32Const(0), call(1)))
	code, err = mod.Run(t.Context(), &engine.RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, code)

	var stdout bytes.Buffer

	_, mod = command(t, writeReady())
	code, err = mod.Run(t.Context(), &engine.RunOptions{Stdout: &stdout, Args: []string{"-x"}, Env: []string{"A=b"}})
	require.NoError(t, err, "returning from _start is exiting with 0")
	assert.Zero(t, code)
	assert.Equal(t, "ready", stdout.String())

	_, mod = command(t, unreachable())
	_, err = mod.Run(t.Context(), &engine.RunOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "running fake: ")
	assert.Contains(t, err.Error(), "unreachable")
}

// TestRunStopsWithItsContextAndItsRuntime runs a command that spins: it fails with ctx's error when ctx ends, and
// with ErrStopped when its runtime closes, during the run or before it.
func TestRunStopsWithItsContextAndItsRuntime(t *testing.T) {
	t.Parallel()

	_, mod := command(t, spin())

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	_, err := mod.Run(ctx, &engine.RunOptions{})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	rt, mod := command(t, slices.Concat(writeReady(), spin()))
	ready := &signal{written: make(chan struct{})}
	done := make(chan error, 1)

	go func() {
		_, runErr := mod.Run(context.Background(), &engine.RunOptions{Stdout: ready})
		done <- runErr
	}()

	<-ready.written
	require.NoError(t, rt.Close(t.Context()))
	require.ErrorIs(t, <-done, engine.ErrStopped)

	_, err = mod.Run(t.Context(), &engine.RunOptions{})
	require.ErrorIs(t, err, engine.ErrStopped, "a closed runtime runs nothing")
}
