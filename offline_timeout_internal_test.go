//go:build integration

package plg

import (
	"context"
	"log/slog"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/directive"
	"github.com/heynemann/go-plg/internal/offline"
)

// stallingHandler is a slog handler that holds the goroutine logging a record whose message is msg for stall on
// the clock of its synctest bubble, and counts the records it held: the step logging it lasts that long, by a
// clock that moves only once every other goroutine of the bubble waits.
type stallingHandler struct {
	msg   string
	stall time.Duration
	held  atomic.Int32
}

func (*stallingHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (h *stallingHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == h.msg {
		h.held.Add(1)
		time.Sleep(h.stall) // the bubble's fake clock: the step's duration, not a wait for another goroutine
	}

	return nil
}

func (h *stallingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *stallingHandler) WithGroup(string) slog.Handler { return h }

// TestCheckOfflineTimeoutBoundsCreateOnly checks a TypeScript plugin as plg validate does, under a 1s timeout, in
// a synctest bubble whose logger holds a step for an hour of the bubble's clock. Held at each guest compile (the
// bundler's, then the guest's, after the bundling: everything before the instance starts), the check passes two
// hours later, so the timeout bounds neither the compiles nor the bundling. Held at the line Create prints, the
// check fails with the deadline, so the timeout bounds Create. (The instantiation between them prints nothing to
// hold: launcher.start leaves it out of the timeout by construction, starting the timeout at Create.)
func TestCheckOfflineTimeoutBoundsCreateOnly(t *testing.T) {
	t.Parallel()

	const timeout, stall = time.Second, time.Hour

	spec := offlineSpec(t, reflect.TypeFor[offlineIface](), fstest.MapFS{"c.go": {Data: []byte("package c\n")}})
	spec.Module = contractref.Module{Path: "github.com/heynemann/go-plg", Version: "v1.0.0"}
	plugin := &offline.Plugin{
		Key: "p",
		Files: fstest.MapFS{"plugin.ts": {Data: []byte(
			"export function Create() {\n  console.log(\"creating\");\n  return { Do: (s: string) => s };\n}\n")}},
		Manifest: []byte("implements: github.com/heynemann/go-plg.offlineIface@v1.0.0\n" +
			"language: typescript\nentrypoint: plugin.ts\n"),
	}

	tests := []struct {
		msg     string
		failure string // empty: the check passes
		held    int32
	}{
		{msg: "plg: guest compiled", held: 2},
		{msg: "plg: guest output", held: 1, failure: "deadline exceeded"},
	}

	for _, tc := range tests {
		synctest.Test(t, func(t *testing.T) {
			handler := &stallingHandler{msg: tc.msg, stall: stall}
			opts := &offline.Options{
				Logger: slog.New(handler),
				Limits: directive.Limits{Timeout: timeout},
			}

			start := time.Now()
			report, err := offline.Check(t.Context(), spec, plugin, opts)
			took := time.Since(start)

			require.NoError(t, err, tc.msg)
			assert.Equal(t, tc.held, handler.held.Load(), tc.msg)
			assert.Equal(t, time.Duration(tc.held)*stall, took, "%s: the held steps are all the bubble's clock saw",
				tc.msg)

			if tc.failure == "" {
				assert.Nil(t, report.Error, tc.msg)

				return
			}

			require.NotNil(t, report.Error, tc.msg)
			assert.Contains(t, *report.Error, tc.failure, tc.msg)
		})
	}
}
