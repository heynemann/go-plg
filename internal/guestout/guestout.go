// Package guestout captures a guest instance's stdout and stderr. Complete lines go to slog, tagged with the
// plugin key and the stream (and the trace and span IDs of the step's span), within a budget per step; the last
// bytes of stderr are kept so that a failure can report what the guest wrote, such as its traceback.
//
// A Capture belongs to one instance and, like the instance, is used by one goroutine at a time.
package guestout

import (
	"bytes"
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// Bounds.
const (
	// MaxLine is the longest line logged in one record; longer lines are logged in pieces.
	MaxLine = 4 << 10
	// StepBudget is how many bytes of each stream are logged per step; the rest is dropped, with one notice.
	// Every record costs its line's length plus one (its newline, or its cut), so a step also logs at most
	// StepBudget records per stream, however many empty lines the guest writes.
	StepBudget = 64 << 10
	// TailSize is how many of the last bytes of stderr [Capture.Guest] can return.
	TailSize = 32 << 10
)

// Capture collects one instance's output. Create it with [New].
type Capture struct {
	logger *slog.Logger
	// ctx is the context of the step in progress, from Begin to End: the guest writes inside a step's host
	// function, which hands its writer no context, and the records are logged with the step's.
	ctx     context.Context //nolint:containedctx // the step's, see above; nil between steps
	key     string
	tail    []byte // ring of the last TailSize bytes of stderr, allocated on the first write
	stdout  stream
	stderr  stream
	written uint64 // bytes ever written to stderr
	mark    uint64 // written when the current step began
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

// New returns the capture of an instance of the plugin with the given key.
func New(logger *slog.Logger, key string) *Capture {
	capture := &Capture{logger: logger, key: key}
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

// Guest returns what stderr received since the step began: its last [TailSize] bytes at most.
func (c *Capture) Guest() string {
	n := min(c.written-c.mark, TailSize)
	if n == 0 {
		return ""
	}

	out := make([]byte, n)
	k := copy(out, c.tail[(c.written-n)%TailSize:])
	copy(out[k:], c.tail)

	return string(out)
}

// keepTail appends data to the ring of stderr's last bytes.
func (c *Capture) keepTail(data []byte) {
	if c.tail == nil {
		c.tail = make([]byte, TailSize)
	}

	size := uint64(len(data))
	if size > TailSize {
		c.written += size - TailSize
		data = data[size-TailSize:]
		size = TailSize
	}

	n := copy(c.tail[c.written%TailSize:], data)
	copy(c.tail, data[n:])

	c.written += size
}

// Write logs every complete line of data and keeps the rest for the next write. It never fails. The lines are
// logged with the context of the step in progress (see [Capture.Begin]).
//
// Its cost is linear in len(data) at most, since the guest's write runs in a host function that no deadline
// interrupts: a newline is searched for only within the line being filled, and once the step's budget and its
// truncation notice are spent the rest of the step is dropped unread (stderr's tail is still kept).
func (s *stream) Write(data []byte) (int, error) {
	if s.keep {
		s.capture.keepTail(data)
	}

	ctx := s.capture.ctx
	if ctx == nil { // written outside a step
		ctx = context.Background()
	}

	for rest := data; len(rest) > 0 && !s.dropped; {
		room := MaxLine - len(s.line)
		end := bytes.IndexByte(rest[:min(len(rest), room+1)], '\n')

		if end < 0 && len(rest) > room {
			s.line = append(s.line, rest[:room]...)
			s.emit(ctx)

			rest = rest[room:]

			continue
		}

		if end < 0 {
			s.line = append(s.line, rest...)

			break
		}

		s.line = append(s.line, rest[:end]...)
		s.emit(ctx)

		rest = rest[end+1:]
	}

	return len(data), nil
}

func (s *stream) flush(ctx context.Context) {
	if len(s.line) > 0 {
		s.emit(ctx)
	}
}

// emit logs the buffered line within the step's budget, with ctx, the step's context, and the IDs of its span.
func (s *stream) emit(ctx context.Context) {
	line := s.line
	s.line = s.line[:0]

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
		slog.String("key", s.capture.key), slog.String("stream", s.name), slog.String("line", string(line)), {}, {},
	}
	n := 3

	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		attrs[3], attrs[4] = slog.String("trace_id", span.TraceID().String()),
			slog.String("span_id", span.SpanID().String())
		n = 5
	}

	s.capture.logger.LogAttrs(ctx, s.level, "plg: guest output", attrs[:n]...)
}
