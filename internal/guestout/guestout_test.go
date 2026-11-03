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
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/guestout"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// record is one logged line.
type record struct {
	msg    string
	key    string
	stream string
	line   string
	level  slog.Level
}

// lines is a slog handler keeping every record.
type lines struct {
	records []record
}

func (*lines) Enabled(context.Context, slog.Level) bool { return true }
func (l *lines) WithAttrs([]slog.Attr) slog.Handler     { return l }
func (l *lines) WithGroup(string) slog.Handler          { return l }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (l *lines) Handle(_ context.Context, rec slog.Record) error {
	got := record{level: rec.Level, msg: rec.Message}

	rec.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "key":
			got.key = a.Value.String()
		case "stream":
			got.stream = a.Value.String()
		case "line":
			got.line = a.Value.String()
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
	handler := &lines{}

	return guestout.New(slog.New(handler), "fooer/p"), handler
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
	capture.Begin()

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

func TestLongLinesAreLoggedInPieces(t *testing.T) {
	t.Parallel()

	capture, logs := newCapture()
	capture.Begin()

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

	capture.Begin()

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

	capture.Begin()
	write(t, capture.Stderr(), "next step\n")
	assert.Equal(t, "next step", logs.texts("stderr")[perStep+1], "Begin renews the budget")
}

func TestEmptyLinesCountAgainstTheBudget(t *testing.T) {
	t.Parallel()

	capture, logs := newCapture()
	capture.Begin()

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
	capture.Begin()
	assert.Empty(t, capture.Guest(), "a new step starts empty")

	write(t, capture.Stdout(), "not stderr\n")
	write(t, capture.Stderr(), "panic: ", "boom\n")
	assert.Equal(t, "panic: boom\n", capture.Guest())
}

func TestGuestKeepsTheLastBytesOfALongStep(t *testing.T) {
	t.Parallel()

	capture, _ := newCapture()
	capture.Begin()

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

	capture.Begin()
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
	capture.Begin()

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
	capture := guestout.New(slog.New(slog.DiscardHandler), "fooer/p")
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		capture.Begin()
		capture.End(ctx)
	}
}

// BenchmarkStderrLine is one traceback-sized line written to stderr: kept in the tail and logged.
func BenchmarkStderrLine(b *testing.B) {
	capture := guestout.New(slog.New(slog.DiscardHandler), "fooer/p")
	line := []byte("/gopath/src/plg.local/plugin/plugin.go:64:2: panic: plg.local/plugin.deeper(...)\n")

	b.ReportAllocs()

	for b.Loop() {
		capture.Begin() // a fresh step: within the budget, so the line is logged

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
			capture := guestout.New(slog.New(slog.DiscardHandler), "fooer/p")
			data := bytes.Repeat([]byte("x"), mib<<20)

			b.SetBytes(int64(len(data)))
			b.ReportAllocs()

			for b.Loop() {
				capture.Begin()

				_, err := capture.Stderr().Write(data)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
