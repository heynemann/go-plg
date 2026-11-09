// Package guestout captures a guest instance's stdout and stderr. Complete lines go to slog, tagged with the
// plugin key and the stream (and the trace and span IDs of the step's span), within a budget per step; the last
// bytes of stderr are kept so that a failure can report what the guest wrote, such as its traceback. Both are
// scrubbed of the plugin's secrets ([redact.Redactor]).
//
// A Capture belongs to one instance and, like the instance, is used by one goroutine at a time.
package guestout

import (
	"bytes"
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/heynemann/go-plg/internal/redact"
)

// Bounds.
const (
	// MaxLine is the longest line logged in one record; longer lines are logged in pieces. When the plugin's
	// secrets hold a value longer than half of it, a piece holds twice that value's length instead, so that a value
	// is never split across pieces, unscrubbed (see [redact.Redactor.Span]).
	MaxLine = 4 << 10
	// StepBudget is how many bytes of each stream are logged per step; the rest is dropped, with one notice.
	// Every record costs its line's length plus one (its newline, or its cut), so a step also logs at most
	// StepBudget records per stream, however many empty lines the guest writes.
	StepBudget = 64 << 10
	// TailSize is how many of the last bytes of stderr [Capture.Guest] can return. The ring keeping them holds
	// twice the longest value of the plugin's secrets instead when that is more, so that the tail never starts
	// within a value it cannot tell.
	TailSize = 32 << 10
)

// Capture collects one instance's output. Create it with [New].
type Capture struct {
	logger *slog.Logger
	// ctx is the context of the step in progress, from Begin to End: the guest writes inside a step's host
	// function, which hands its writer no context, and the records are logged with the step's.
	ctx     context.Context  //nolint:containedctx // the step's, see above; nil between steps
	secrets *redact.Redactor // scrubs lines and the tail; nil scrubs nothing
	key     string
	tail    []byte // ring of the last bytes of stderr (TailSize at least), allocated on the first write
	stdout  stream
	stderr  stream
	written uint64 // bytes ever written to stderr
	mark    uint64 // written when the current step began
	held    uint64 // how many of the ring's bytes were written to it (fewer than its size until it fills)
}

// stream is one output stream: the partial line not logged yet and the step's logging budget.
type stream struct {
	capture *Capture
	name    string
	line    []byte
	logged  int  // bytes logged in the current step
	dropped bool // the current step's truncation notice was logged
	keep    bool // the stream's bytes also go to the tail (stderr)
	level   slog.Level
}

// New returns the capture of an instance of the plugin with the given key, scrubbing what it reports of secrets'
// values (nil: none).
func New(logger *slog.Logger, key string, secrets *redact.Redactor) *Capture {
	capture := &Capture{logger: logger, key: key, secrets: secrets}
	capture.stdout = stream{capture: capture, name: "stdout", level: slog.LevelInfo}
	capture.stderr = stream{capture: capture, name: "stderr", level: slog.LevelWarn, keep: true}

	return capture
}

// Stdout returns the writer of the guest's standard output.
func (c *Capture) Stdout() io.Writer {
	return &c.stdout
}

// Stderr returns the writer of the guest's standard error.
func (c *Capture) Stderr() io.Writer {
	return &c.stderr
}

// Begin starts a step of the instance (an init, call or close) running under ctx: [Capture.Guest] returns what
// stderr receives from now on, each stream's logging budget starts afresh, and the lines are logged with ctx
// (with trace_id and span_id when it holds a span).
func (c *Capture) Begin(ctx context.Context) {
	c.ctx = ctx
	c.mark = c.written
	c.stdout.logged, c.stdout.dropped = 0, false
	c.stderr.logged, c.stderr.dropped = 0, false
}

// End ends a step, logging the partial lines it left with ctx, the step's context.
func (c *Capture) End(ctx context.Context) {
	c.stdout.flush(ctx)
	c.stderr.flush(ctx)

	c.ctx = nil
}

// Guest returns what stderr received since the step began, scrubbed: its last [TailSize] bytes at most. When the
// step wrote more than the ring holds, a start that may be the end of a secret's value is left out.
func (c *Capture) Guest() string {
	n := min(c.written-c.mark, c.held)
	if n == 0 {
		return ""
	}

	out := c.last(n)
	if c.written-c.mark > n {
		out = out[c.secrets.Trim(out):]
	}

	text := c.secrets.String(string(out))

	return text[max(0, len(text)-TailSize):]
}

// last returns a copy of the last n bytes of the ring, n at most c.held.
func (c *Capture) last(n uint64) []byte {
	if n == 0 {
		return nil
	}

	out := make([]byte, n)
	k := copy(out, c.tail[(c.written-n)%uint64(len(c.tail)):])
	copy(out[k:], c.tail)

	return out
}

// keepTail appends data to the ring of stderr's last bytes, first growing the ring to twice the longest secret
// value when that is more than it holds.
func (c *Capture) keepTail(data []byte) {
	if size := c.secrets.Span(TailSize); len(c.tail) < size {
		kept := c.last(c.held)
		c.tail = make([]byte, size)
		c.place(c.written-c.held, kept)
	}

	ring := uint64(len(c.tail))

	size := uint64(len(data))
	if size > ring {
		c.written += size - ring
		data = data[size-ring:]
		size = ring
	}

	c.place(c.written, data)

	c.written += size
	c.held = min(c.held+size, ring)
}

// place copies data, at most the ring's size, into the ring from position at of stderr's bytes on.
func (c *Capture) place(at uint64, data []byte) {
	n := copy(c.tail[at%uint64(len(c.tail)):], data)
	copy(c.tail, data[n:])
}

// Write logs every complete line of data and keeps the rest for the next write. It never fails. The lines are
// logged with the context of the step in progress (see [Capture.Begin]). A line longer than [MaxLine] (or than
// twice the longest secret value, when that is more) is logged in pieces, cut where no secret's value is split.
//
// Its cost is linear in len(data) at most, since the guest's write runs in a host function that no deadline
// interrupts: a newline is searched for only within the line being filled, and once the step's budget and its
// truncation notice are spent the rest of the step is dropped unread (stderr's tail is still kept). A cut keeps
// less than half a piece for the next one.
func (s *stream) Write(data []byte) (int, error) {
	if s.keep {
		s.capture.keepTail(data)
	}

	ctx := s.capture.ctx
	if ctx == nil { // written outside a step
		ctx = context.Background()
	}

	for rest := data; len(rest) > 0 && !s.dropped; {
		// A piece holds the longest secret value twice, so that a value it cuts fits whole in the next one. The
		// line may already be longer when a rotation dropped the longest value.
		room := max(0, s.capture.secrets.Span(MaxLine)-len(s.line))
		end := bytes.IndexByte(rest[:min(len(rest), room+1)], '\n')

		if end < 0 && len(rest) > room {
			s.line = append(s.line, rest[:room]...)
			rest = rest[room:]

			cut := s.capture.secrets.Cut(s.line)
			s.emit(ctx, s.line[:cut])

			s.line = s.line[:copy(s.line, s.line[cut:])]

			continue
		}

		if end < 0 {
			s.line = append(s.line, rest...)

			break
		}

		s.line = append(s.line, rest[:end]...)
		s.emit(ctx, s.line)

		s.line = s.line[:0]
		rest = rest[end+1:]
	}

	return len(data), nil
}

func (s *stream) flush(ctx context.Context) {
	if len(s.line) > 0 {
		s.emit(ctx, s.line)

		s.line = s.line[:0]
	}
}

// emit logs line, scrubbed, within the step's budget, with ctx, the step's context, and the IDs of its span.
func (s *stream) emit(ctx context.Context, line []byte) {
	if s.logged >= StepBudget {
		if !s.dropped {
			s.dropped = true
			s.capture.logger.LogAttrs(ctx, slog.LevelWarn, "plg: guest output truncated",
				slog.String("key", s.capture.key), slog.String("stream", s.name), slog.Int("budget", StepBudget))
		}

		return
	}

	s.logged += len(line) + 1

	attrs := [...]slog.Attr{
		slog.String("key", s.capture.key), slog.String("stream", s.name),
		slog.String("line", s.capture.secrets.String(string(line))),
		{},
		{},
	}
	n := 3

	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		attrs[3], attrs[4] = slog.String("trace_id", span.TraceID().String()),
			slog.String("span_id", span.SpanID().String())
		n = 5
	}

	s.capture.logger.LogAttrs(ctx, s.level, "plg: guest output", attrs[:n]...)
}
