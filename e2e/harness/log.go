package harness

import (
	"strings"
	"sync/atomic"

	"github.com/puzpuzpuz/xsync/v4"
)

// Log is the destination of a platform's log handler (slog's), which a test reads while the platform still
// writes: every Write is kept whole, in order. Safe for concurrent use.
type Log struct {
	writes *xsync.Map[int64, string]
	n      atomic.Int64
}

// NewLog returns an empty Log.
func NewLog() *Log {
	return &Log{writes: xsync.NewMap[int64, string]()}
}

// Write keeps p.
func (l *Log) Write(p []byte) (int, error) {
	l.writes.Store(l.n.Add(1), string(p))

	return len(p), nil
}

// String returns what was written so far, in order.
func (l *Log) String() string {
	var text strings.Builder

	for i := range l.n.Load() {
		write, _ := l.writes.Load(i + 1)
		text.WriteString(write)
	}

	return text.String()
}
