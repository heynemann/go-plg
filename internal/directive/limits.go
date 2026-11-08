package directive

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// The ranges plg accepts for a limit, here and in its Register options (plg.WithMemoryLimit, plg.WithPool).
const (
	// MinMemory is the smallest memory limit: one 64 KiB wasm page.
	MinMemory = 1 << 16
	// MaxMemory is the largest memory limit: 4 GiB, the whole 32-bit wasm address space.
	MaxMemory = 1 << 32
	// MaxPool is the largest pool maximum. Each pool keeps room for that many idle instances, so it is capped.
	MaxPool = 4096
)

// memoryUnits are the suffixes a memory value may carry, binary (IEC) units, longest first; none means bytes.
var memoryUnits = []struct { //nolint:gochecknoglobals // a constant table
	suffix string
	shift  uint
}{{"GiB", 30}, {"MiB", 20}, {"KiB", 10}, {"B", 0}}

// Limits are the resource limits plugins run under. A zero field is unset: whoever applies them falls back to
// another source (a contract's directive, then plg's defaults).
type Limits struct {
	// Memory is the memory limit of each instance, in bytes (plg.WithMemoryLimit).
	Memory uint64
	// Timeout bounds each call, Create included.
	Timeout time.Duration
	// PoolMin and PoolMax are the pool's bounds (plg.WithPool); both are set or neither is.
	PoolMin, PoolMax int
}

// limitKeys parses each limit's value into l, reporting false for a malformed or out-of-range value, and says
// what it wants.
//
//nolint:gochecknoglobals // a constant table
var limitKeys = map[string]struct {
	set  func(l *Limits, value string) bool
	want string
}{
	"memory": {
		set: func(l *Limits, value string) bool {
			n, ok := parseMemory(value)
			if ok {
				l.Memory = n
			}

			return ok
		},
		want: "bytes or a number of B, KiB, MiB or GiB from " + FormatMemory(MinMemory) + " (one wasm page) to " +
			FormatMemory(MaxMemory) + ", as in 64MiB",
	},
	"timeout": {
		set: func(l *Limits, value string) bool {
			d, err := time.ParseDuration(value)
			if err == nil && d > 0 {
				l.Timeout = d
			}

			return err == nil && d > 0
		},
		want: "a positive duration, as in 2s or 500ms",
	},
	"pool": {
		set: func(l *Limits, value string) bool {
			pool, ok := parsePool(value)
			if ok {
				l.PoolMin, l.PoolMax = pool.PoolMin, pool.PoolMax
			}

			return ok
		},
		want: "MIN..MAX or N with 1 <= MIN <= MAX <= " + strconv.Itoa(MaxPool) + ", as in 1..8",
	},
}

// Set sets the limit key from its textual value, as `//plg:limits` writes it:
//   - memory: bytes, or a number of B, KiB, MiB or GiB (64MiB), from [MinMemory] to [MaxMemory];
//   - timeout: a positive Go duration (2s, 1m30s);
//   - pool: MIN..MAX or N (MIN..MAX = N..N), with 1 <= MIN <= MAX <= [MaxPool].
//
// Errors wrap [ErrDirective] and leave l as it was.
func (l *Limits) Set(key, value string) error {
	limit, known := limitKeys[key]
	if !known {
		return fmt.Errorf("%w: unknown limit %q (want memory, timeout or pool)", ErrDirective, key)
	}

	if !limit.set(l, value) {
		return fmt.Errorf("%w: %s=%s: want %s", ErrDirective, key, value, limit.want)
	}

	return nil
}

// Override returns l with the limits other sets replacing its own.
func (l *Limits) Override(other Limits) Limits {
	limits := *l

	if other.Memory != 0 {
		limits.Memory = other.Memory
	}

	if other.Timeout != 0 {
		limits.Timeout = other.Timeout
	}

	if other.PoolMax != 0 {
		limits.PoolMin, limits.PoolMax = other.PoolMin, other.PoolMax
	}

	return limits
}

// String writes the limits l sets as `//plg:limits` does (memory=64MiB timeout=2s pool=1..8), in that order;
// "none" when it sets none.
func (l *Limits) String() string {
	var parts []string

	if l.Memory != 0 {
		parts = append(parts, "memory="+FormatMemory(l.Memory))
	}

	if l.Timeout != 0 {
		parts = append(parts, "timeout="+l.Timeout.String())
	}

	if l.PoolMax != 0 {
		parts = append(parts, fmt.Sprintf("pool=%d..%d", l.PoolMin, l.PoolMax))
	}

	if len(parts) == 0 {
		return "none"
	}

	return strings.Join(parts, " ")
}

// FormatMemory writes a number of bytes in the largest unit that divides it exactly, as [Limits.Set] reads it.
func FormatMemory(n uint64) string {
	for _, unit := range memoryUnits {
		if n != 0 && n%(1<<unit.shift) == 0 {
			return strconv.FormatUint(n>>unit.shift, 10) + unit.suffix
		}
	}

	return "0B"
}

// parseMemory reads a memory value; it reports false for a malformed one or one out of range.
func parseMemory(value string) (uint64, bool) {
	digits, shift := value, uint(0)

	for _, unit := range memoryUnits {
		if trimmed, ok := strings.CutSuffix(value, unit.suffix); ok {
			digits, shift = trimmed, unit.shift

			break
		}
	}

	n, ok := parseDigits(digits)
	if !ok {
		return 0, false
	}

	hi, bytes := bits.Mul64(n, 1<<shift)

	return bytes, hi == 0 && bytes >= MinMemory && bytes <= MaxMemory
}

// parsePool reads MIN..MAX or N into the pool bounds of limits, within the range plg accepts.
func parsePool(value string) (Limits, bool) {
	lo, hi, isRange := strings.Cut(value, "..")
	if !isRange {
		hi = lo
	}

	minimum, okMin := parseCount(lo)
	maximum, okMax := parseCount(hi)

	return Limits{PoolMin: minimum, PoolMax: maximum},
		okMin && okMax && minimum >= 1 && minimum <= maximum && maximum <= MaxPool
}

// parseCount reads a count written with decimal digits only (no sign, no underscores).
func parseCount(value string) (int, bool) {
	if !digitsOnly(value) {
		return 0, false
	}

	n, err := strconv.Atoi(value)

	return n, err == nil
}

// parseDigits reads a number written with decimal digits only.
func parseDigits(value string) (uint64, bool) {
	if !digitsOnly(value) {
		return 0, false
	}

	n, err := strconv.ParseUint(value, 10, 64)

	return n, err == nil
}

// digitsOnly reports whether value is decimal digits, at least one.
func digitsOnly(value string) bool {
	return value != "" && strings.TrimLeft(value, "0123456789") == ""
}
