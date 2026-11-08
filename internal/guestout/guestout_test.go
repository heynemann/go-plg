package guestout_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/guestout"
	"github.com/heynemann/go-plg/internal/redact"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// record is one logged line.
type record struct {
	ctx     context.Context //nolint:containedctx // the context the record was logged with, for the test to check
	msg     string
	key     string
	stream  string
	line    string
	traceID string
	spanID  string
	level   slog.Level
}

// lines is a slog handler keeping every record.
type lines struct {
	records []record
}

func (*lines) Enabled(context.Context, slog.Level) bool { return true }
func (l *lines) WithAttrs([]slog.Attr) slog.Handler     { return l }
func (l *lines) WithGroup(string) slog.Handler          { return l }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (l *lines) Handle(ctx context.Context, rec slog.Record) error {
	got := record{ctx: ctx, level: rec.Level, msg: rec.Message}

	rec.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "key":
			got.key = attr.Value.String()
		case "stream":
			got.stream = attr.Value.String()
		case "line":
			got.line = attr.Value.String()
		case "trace_id":
			got.traceID = attr.Value.String()
		case "span_id":
			got.spanID = attr.Value.String()
		default: // the budget of a truncation notice
		}

		return true
	})

	l.records = append(l.records, got)

	return nil
}

// texts returns the logged lines of one stream.
func (l *lines) texts(stream string) []string {
	var out []string

	for _, r := range l.records {
		if r.stream == stream && r.msg == "plg: guest output" {
			out = append(out, r.line)
		}
	}

	return out
}

func newCapture() (*guestout.Capture, *lines) {
	return newScrubbingCapture(nil)
}

// newScrubbingCapture returns a capture scrubbing the values secrets holds, and its records.
func newScrubbingCapture(secrets *redact.Redactor) (*guestout.Capture, *lines) {
	handler := &lines{}

	return guestout.New(slog.New(handler), "fooer/p", secrets), handler
}

func write(t *testing.T, w io.Writer, chunks ...string) {
	t.Helper()

	for _, chunk := range chunks {
		n, err := w.Write([]byte(chunk))
		require.NoError(t, err)
		require.Equal(t, len(chunk), n, "writes never fail or shorten")
	}
}

func TestLinesAreLoggedWithTheKeyAndStream(t *testing.T) {
	t.Parallel()

	capture, logs := newCapture()
	capture.Begin(t.Context())

	write(t, capture.Stdout(), "hello ", "world\nsecond\n\nthird")
	write(t, capture.Stderr(), "oops\n")
	assert.Equal(t, []string{"hello world", "second", ""}, logs.texts("stdout"), "a partial line waits")

	capture.End(t.Context())
	assert.Equal(t, []string{"hello world", "second", "", "third"}, logs.texts("stdout"), "End flushes it")
	assert.Equal(t, []string{"oops"}, logs.texts("stderr"))

	for _, r := range logs.records {
		assert.Equal(t, "fooer/p", r.key)

		want := slog.LevelInfo
		if r.stream == "stderr" {
			want = slog.LevelWarn
		}

		assert.Equal(t, want, r.level, r.stream)
	}

	capture.End(t.Context())
	assert.Len(t, logs.records, 5, "nothing pending: End logs nothing")
}

// stepKey marks the context of a step, for TestLinesCarryTheirStepsContextAndSpan.
type stepKey struct{}

func TestLinesCarryTheirStepsContextAndSpan(t *testing.T) {
	t.Parallel()

	span := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 15: 0x01},
		SpanID:     trace.SpanID{0x0c, 7: 0x02},
		TraceFlags: trace.FlagsSampled,
	})
	traced := trace.ContextWithSpanContext(context.WithValue(t.Context(), stepKey{}, "call"), span)

	capture, logs := newCapture()
	write(t, capture.Stdout(), "before any step\n")

	capture.Begin(traced)
	write(t, capture.Stdout(), "in the call\npartial")
	capture.End(context.WithValue(traced, stepKey{}, "end"))

	capture.Begin(context.WithValue(t.Context(), stepKey{}, "untraced"))
	write(t, capture.Stderr(), "untraced\n")
	capture.End(t.Context())
	write(t, capture.Stdout(), "after the steps\n")

	require.Len(t, logs.records, 5)

	want := []struct {
		step            any
		traceID, spanID string
	}{
		{step: nil},
		{step: "call", traceID: "0a0b0000000000000000000000000001", spanID: "0c00000000000002"},
		{step: "end", traceID: "0a0b0000000000000000000000000001", spanID: "0c00000000000002"},
		{step: "untraced"},
		{step: nil},
	}

	for i, rec := range logs.records {
		assert.Equal(t, want[i].step, rec.ctx.Value(stepKey{}), "record %d (%s) is logged with its step's context",
			i, rec.line)
		assert.Equal(t, want[i].traceID, rec.traceID, "record %d (%s)", i, rec.line)
		assert.Equal(t, want[i].spanID, rec.spanID, "record %d (%s)", i, rec.line)
	}
}

func TestLongLinesAreLoggedInPieces(t *testing.T) {
	t.Parallel()

	capture, logs := newCapture()
	capture.Begin(t.Context())

	exact := strings.Repeat("a", guestout.MaxLine)
	write(t, capture.Stdout(), exact+"\n")
	assert.Equal(t, []string{exact}, logs.texts("stdout"), "a line of exactly MaxLine bytes is one record")

	write(t, capture.Stdout(), strings.Repeat("b", guestout.MaxLine-1), "bb", "b\n")
	assert.Equal(t, []string{exact, strings.Repeat("b", guestout.MaxLine), "bb"}, logs.texts("stdout"),
		"a longer line is cut at MaxLine, across writes")

	write(t, capture.Stdout(), strings.Repeat("c", guestout.MaxLine), "\n")
	assert.Equal(t, strings.Repeat("c", guestout.MaxLine), logs.texts("stdout")[3],
		"a full buffer followed by its newline is one record, not one plus an empty one")
	assert.Len(t, logs.texts("stdout"), 4)
}

func TestEachStepLogsWithinItsBudget(t *testing.T) {
	t.Parallel()

	capture, logs := newCapture()
	line := strings.Repeat("x", 1000) + "\n"
	perStep := guestout.StepBudget / len(line)

	capture.Begin(t.Context())

	for range 2 * perStep {
		write(t, capture.Stderr(), line)
	}

	capture.End(t.Context())

	logged := logs.texts("stderr")
	assert.Len(t, logged, perStep+1, "lines are logged until the budget is reached, the one crossing it included")

	var notices int

	for _, r := range logs.records {
		if r.msg == "plg: guest output truncated" {
			notices++

			assert.Equal(t, "stderr", r.stream)
			assert.Equal(t, slog.LevelWarn, r.level)
		}
	}

	assert.Equal(t, 1, notices, "one notice per step")

	write(t, capture.Stdout(), "other stream\n")
	assert.Equal(t, []string{"other stream"}, logs.texts("stdout"), "each stream has its own budget")

	capture.Begin(t.Context())
	write(t, capture.Stderr(), "next step\n")
	assert.Equal(t, "next step", logs.texts("stderr")[perStep+1], "Begin renews the budget")
}

func TestEmptyLinesCountAgainstTheBudget(t *testing.T) {
	t.Parallel()

	capture, logs := newCapture()
	capture.Begin(t.Context())

	write(t, capture.Stdout(), strings.Repeat("\n", 4_000_000))
	capture.End(t.Context())

	assert.Len(t, logs.texts("stdout"), guestout.StepBudget, "each record costs its newline")
	assert.Len(t, logs.records, guestout.StepBudget+1, "the records, then one truncation notice")
	assert.Equal(t, "plg: guest output truncated", logs.records[len(logs.records)-1].msg)
}

func TestGuestReturnsTheStepsStderr(t *testing.T) {
	t.Parallel()

	capture, _ := newCapture()
	assert.Empty(t, capture.Guest(), "nothing written yet")

	write(t, capture.Stderr(), "from instantiation\n")
	capture.Begin(t.Context())
	assert.Empty(t, capture.Guest(), "a new step starts empty")

	write(t, capture.Stdout(), "not stderr\n")
	write(t, capture.Stderr(), "panic: ", "boom\n")
	assert.Equal(t, "panic: boom\n", capture.Guest())
}

func TestGuestKeepsTheLastBytesOfALongStep(t *testing.T) {
	t.Parallel()

	capture, _ := newCapture()
	capture.Begin(t.Context())

	var all bytes.Buffer

	for i := range 100 {
		chunk := strings.Repeat(string(rune('a'+i%26)), 1000) + "\n"
		all.WriteString(chunk)
		write(t, capture.Stderr(), chunk)
	}

	want := all.Bytes()[all.Len()-guestout.TailSize:]
	assert.Equal(t, string(want), capture.Guest(), "the ring wrapped: the last TailSize bytes, in order")

	huge := strings.Repeat("0123456789", guestout.TailSize/5) // twice the ring in one write
	write(t, capture.Stderr(), huge)
	assert.Equal(t, huge[len(huge)-guestout.TailSize:], capture.Guest(), "one write larger than the ring")

	capture.Begin(t.Context())
	write(t, capture.Stderr(), "after\n")
	assert.Equal(t, "after\n", capture.Guest(), "only the new step, though the ring holds more")
}

// guarded maps size bytes of which only the first head and the last tail bytes (whole pages) can be read:
// reading anything in between faults.
func guarded(t *testing.T, size, head, tail int) []byte {
	t.Helper()

	page := os.Getpagesize()
	require.Zero(t, size%page+head%page+tail%page, "whole pages")

	data, err := syscall.Mmap(-1, 0, size, syscall.PROT_NONE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, syscall.Munmap(data)) })

	require.NoError(t, syscall.Mprotect(data[:head], syscall.PROT_READ|syscall.PROT_WRITE))
	require.NoError(t, syscall.Mprotect(data[size-tail:], syscall.PROT_READ|syscall.PROT_WRITE))

	return data
}

// A newline-free write far past the budget: only its first bytes, up to the line crossing the budget and the
// one bringing the truncation notice, and its last TailSize bytes may be read. The rest of the 64 MiB is
// unreadable, so a scan for a newline beyond the next line, or any per-line work after the notice, faults.
func TestOutputPastTheBudgetIsNotScanned(t *testing.T) {
	t.Parallel()

	const size, head = 64 << 20, guestout.StepBudget + 2*guestout.MaxLine

	data := guarded(t, size, head, guestout.TailSize)
	copy(data[:head], strings.Repeat("x", head))
	copy(data[size-guestout.TailSize:], strings.Repeat("y", guestout.TailSize))

	capture, logs := newCapture()
	capture.Begin(t.Context())

	var (
		n     int
		err   error
		fault any
	)

	func() {
		defer debug.SetPanicOnFault(debug.SetPanicOnFault(true)) // a fault panics instead of crashing
		defer func() { fault = recover() }()

		n, err = capture.Stderr().Write(data)
	}()
	require.Nil(t, fault, "the write read output past the budget")
	require.NoError(t, err)
	assert.Equal(t, size, n)

	capture.End(t.Context())

	piece := strings.Repeat("x", guestout.MaxLine)
	want := make([]string, guestout.StepBudget/(guestout.MaxLine+1)+1) // the pieces up to the one crossing it

	for i := range want {
		want[i] = piece
	}

	assert.Equal(t, want, logs.texts("stderr"))
	assert.Len(t, logs.records, len(want)+1)
	assert.Equal(t, "plg: guest output truncated", logs.records[len(logs.records)-1].msg)
	assert.Equal(t, strings.Repeat("y", guestout.TailSize), capture.Guest(), "the tail is still kept")
}

// BenchmarkStep is what every call pays for the capture when the guest writes nothing: Begin and End.
func BenchmarkStep(b *testing.B) {
	capture := guestout.New(slog.New(slog.DiscardHandler), "fooer/p", nil)
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		capture.Begin(ctx)
		capture.End(ctx)
	}
}

// BenchmarkStderrLine is one traceback-sized line written to stderr: kept in the tail and logged.
func BenchmarkStderrLine(b *testing.B) {
	capture := guestout.New(slog.New(slog.DiscardHandler), "fooer/p", nil)
	line := []byte("/gopath/src/plg.local/plugin/plugin.go:64:2: panic: plg.local/plugin.deeper(...)\n")

	b.ReportAllocs()

	for b.Loop() {
		capture.Begin(b.Context()) // a fresh step: within the budget, so the line is logged

		_, err := capture.Stderr().Write(line)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNewlineFreeWrite is one step's single write without a newline, of growing size, to stderr: the cost
// grows linearly at most (past the budget only the tail is kept).
func BenchmarkNewlineFreeWrite(b *testing.B) {
	for _, mib := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			capture := guestout.New(slog.New(slog.DiscardHandler), "fooer/p", nil)
			data := bytes.Repeat([]byte("x"), mib<<20)

			b.SetBytes(int64(len(data)))
			b.ReportAllocs()

			for b.Loop() {
				capture.Begin(b.Context())

				_, err := capture.Stderr().Write(data)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// secretValues returns a redactor holding the value tok-123456 of the secret api_key.
func secretValues() *redact.Redactor {
	secrets := &redact.Redactor{}
	secrets.Add("api_key", "tok-123456")

	return secrets
}

func TestSecretsAreScrubbedFromLinesAndTheTail(t *testing.T) {
	t.Parallel()

	capture, logs := newScrubbingCapture(secretValues())
	capture.Begin(t.Context())

	write(t, capture.Stdout(), "token tok-", "123456 sent\n")
	write(t, capture.Stderr(), "failed with tok-123456\n")
	assert.Equal(t, []string{"token [secret api_key] sent"}, logs.texts("stdout"), "a value split across writes")
	assert.Equal(t, []string{"failed with [secret api_key]"}, logs.texts("stderr"))
	assert.Equal(t, "failed with [secret api_key]\n", capture.Guest())

	capture.End(t.Context())
}

func TestCuttingALongLineNeverSplitsASecret(t *testing.T) {
	t.Parallel()

	capture, logs := newScrubbingCapture(secretValues())
	capture.Begin(t.Context())

	// The value straddles MaxLine: a cut at MaxLine would leave "tok-12" and "3456" in two records.
	head := strings.Repeat("a", guestout.MaxLine-6)
	write(t, capture.Stdout(), head+"tok-123456"+strings.Repeat("b", 10)+"\n")
	capture.End(t.Context())

	got := logs.texts("stdout")
	require.Len(t, got, 2)
	assert.Equal(t, head+"[secret api_key]"+strings.Repeat("b", 10), got[0]+got[1])
	assert.Less(t, len(got[0]), len(head)+1, "the cut moved before the value")

	for _, piece := range got {
		assert.NotContains(t, piece, "tok-")
		assert.NotContains(t, piece, "3456")
	}
}

func TestTheTailDropsTheEndOfACutSecret(t *testing.T) {
	t.Parallel()

	capture, _ := newScrubbingCapture(secretValues())
	capture.Begin(t.Context())

	// The tail's first bytes would be "3456", the end of a value whose start fell out of it.
	rest := strings.Repeat("x", guestout.TailSize-4)
	write(t, capture.Stderr(), "tok-123456"+rest)

	got := capture.Guest()
	assert.NotContains(t, got, "3456")
	assert.True(t, strings.HasSuffix(rest, got), "only the start is dropped")
	assert.GreaterOrEqual(t, len(got), len(rest)-len("tok-123456"), "at most a value's length is dropped")

	capture.End(t.Context())
}
