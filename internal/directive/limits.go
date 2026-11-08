// Package directive reads what a contract states for its plugins beside its Go declarations, and the limits plugin
// authors test them under.
package directive

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrDirective reports a malformed directive or limit.
var ErrDirective = errors.New("invalid plg directive")

// Bounds of the limits, as Register's options bound them.
const (
	minMemory = 64 << 10
	maxMemory = 4 << 30
	maxPool   = 4096
)

// Limits are the limits plugins are tested under: a per-instance memory limit, a deadline for each call and for
// Create, and a pool size. A zero field is unset.
type Limits struct {
	// Memory is the memory limit of an instance, in bytes.
	Memory uint64
	// Timeout bounds Create and each call.
	Timeout time.Duration
	// PoolMin and PoolMax are the pool's size.
	PoolMin, PoolMax int
}

// Set sets the limit key from its written value: memory (bytes, or a number with a KiB, MiB or GiB suffix, from
// 64KiB to 4GiB), timeout (a Go duration above zero) or pool (N, or MIN..MAX, from 1 to 4096). It fails with
// [ErrDirective], leaving the limits as they were.
func (l *Limits) Set(key, value string) error {
	next := *l

	var err error

	switch key {
	case "memory":
		err = next.setMemory(value)
	case "timeout":
		err = next.setTimeout(value)
	case "pool":
		err = next.setPool(value)
	default:
		err = fmt.Errorf("%w: unknown limit %q (memory, timeout, pool)", ErrDirective, key)
	}

	if err == nil {
		*l = next
	}

	return err
}

// Override returns the limits with those o sets in place of l's; the pool's size is one limit.
func (l *Limits) Override(o Limits) Limits { //nolint:varnamelen // o is the override, as l the limits
	limits := *l

	if o.Memory != 0 {
		limits.Memory = o.Memory
	}

	if o.Timeout != 0 {
		limits.Timeout = o.Timeout
	}

	if o.PoolMax != 0 {
		limits.PoolMin, limits.PoolMax = o.PoolMin, o.PoolMax
	}

	return limits
}

// String writes the limits that are set as Set reads them, or "none".
func (l *Limits) String() string {
	var set []string

	if l.Memory != 0 {
		set = append(set, "memory="+formatMemory(l.Memory))
	}

	if l.Timeout != 0 {
		set = append(set, "timeout="+l.Timeout.String())
	}

	if l.PoolMax != 0 {
		set = append(set, fmt.Sprintf("pool=%d..%d", l.PoolMin, l.PoolMax))
	}

	if len(set) == 0 {
		return "none"
	}

	return strings.Join(set, " ")
}

// units are the memory suffixes, largest first.
var units = []struct { //nolint:gochecknoglobals // a constant table
	suffix string
	shift  uint
}{{"GiB", 30}, {"MiB", 20}, {"KiB", 10}}

// setMemory sets the memory limit from its written value.
func (l *Limits) setMemory(value string) error {
	number, shift := value, uint(0)

	for _, unit := range units {
		if cut, ok := strings.CutSuffix(value, unit.suffix); ok {
			number, shift = cut, unit.shift

			break
		}
	}

	n, err := strconv.ParseUint(number, 10, 64)
	if err != nil || n > maxMemory>>shift || n<<shift < minMemory {
		return fmt.Errorf("%w: memory must be from 64KiB to 4GiB (bytes, or a number with a KiB, MiB or GiB "+
			"suffix), not %q", ErrDirective, value)
	}

	l.Memory = n << shift

	return nil
}

// setTimeout sets the timeout from its written value.
func (l *Limits) setTimeout(value string) error {
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return fmt.Errorf("%w: timeout must be a duration above zero (like 2s), not %q", ErrDirective, value)
	}

	l.Timeout = timeout

	return nil
}

// setPool sets the pool's size from its written value.
func (l *Limits) setPool(value string) error {
	lowText, highText, ranged := strings.Cut(value, "..")
	if !ranged {
		highText = lowText
	}

	low, lowErr := strconv.Atoi(lowText)
	high, highErr := strconv.Atoi(highText)

	if lowErr != nil || highErr != nil || low < 1 || low > high || high > maxPool {
		return fmt.Errorf("%w: pool must be N or MIN..MAX with 1 <= MIN <= MAX <= 4096, not %q", ErrDirective, value)
	}

	l.PoolMin, l.PoolMax = low, high

	return nil
}

// formatMemory writes a memory limit in its largest whole unit.
func formatMemory(memory uint64) string {
	for _, unit := range units {
		if memory%(1<<unit.shift) == 0 {
			return strconv.FormatUint(memory>>unit.shift, 10) + unit.suffix
		}
	}

	return strconv.FormatUint(memory, 10)
}
