package redact_test

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/redact"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestStringReplacesEveryValueByItsSecretsMask(t *testing.T) {
	t.Parallel()

	var secrets redact.Redactor

	assert.Equal(t, "nothing yet: s3cret", secrets.String("nothing yet: s3cret"), "no value recorded")

	secrets.Add("api_key", "s3cret")
	secrets.Add("token", "s3cret-long") // contains api_key's value: replaced whole
	secrets.Add("empty", "")            // would match everywhere: not recorded
	secrets.Add("again", "s3cret")      // api_key's value too: masked as api_key, which recorded it first

	assert.Equal(t, "a=[secret api_key] b=[secret token] c=[secret api_key][secret api_key]",
		secrets.String("a=s3cret b=s3cret-long c=s3crets3cret"))
	assert.Equal(t, "[secret empty] is no value", redact.Mask("empty")+" is no value")
	assert.Equal(t, "plain", secrets.String("plain"))

	var none *redact.Redactor

	assert.Equal(t, "s3cret", none.String("s3cret"), "a nil redactor scrubs nothing")
	assert.Equal(t, 6, none.Cut([]byte("abcdef")))
	assert.Zero(t, none.Trim([]byte("abcdef")))
}

// TestEachSecretKeepsItsNewestValues: rotations add values; a secret keeps its newest MaxKept, the others theirs.
func TestEachSecretKeepsItsNewestValues(t *testing.T) {
	t.Parallel()

	var secrets redact.Redactor

	secrets.Add("other", "other-value")

	for i := range redact.MaxKept + 2 {
		secrets.Add("rotating", "value-"+strconv.Itoa(i))
	}

	assert.Equal(t, "value-0 value-1", secrets.String("value-0 value-1"), "the two oldest are gone")

	for i := 2; i < redact.MaxKept+2; i++ {
		assert.Equal(t, "[secret rotating]", secrets.String("value-"+strconv.Itoa(i)))
	}

	assert.Equal(t, "[secret other]", secrets.String("other-value"), "another secret keeps its value")
}

// TestAValueTwoSecretsHoldOutlivesEitherRotating: a value recorded under two secrets is a value of each, so one of
// them rotating away from it leaves it scrubbed as the other's.
func TestAValueTwoSecretsHoldOutlivesEitherRotating(t *testing.T) {
	t.Parallel()

	var secrets redact.Redactor

	secrets.Add("a", "shared")
	secrets.Add("b", "shared")

	for i := range redact.MaxKept {
		secrets.Add("a", "a-value-"+strconv.Itoa(i))
	}

	assert.Equal(t, "b is [secret b]", secrets.String("b is shared"))
}

// TestAValueResolvedAgainBecomesNewest: a secret resolving a value it had before keeps it as its newest, so later
// rotations evict the values older than it instead.
func TestAValueResolvedAgainBecomesNewest(t *testing.T) {
	t.Parallel()

	var secrets redact.Redactor

	secrets.Add("a", "xval")

	for i := 1; i < redact.MaxKept; i++ {
		secrets.Add("a", "y"+strconv.Itoa(i))
	}

	secrets.Add("a", "xval")
	secrets.Add("a", "y8")

	assert.Equal(t, "[secret a] y1", secrets.String("xval y1"), "y1 is the oldest, xval the second newest")
}

func TestAddIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	var (
		secrets redact.Redactor
		wg      sync.WaitGroup
	)

	for w := range 8 {
		wg.Go(func() {
			for i := range 50 {
				secrets.Add("s"+strconv.Itoa(w), "v"+strconv.Itoa(w)+"-"+strconv.Itoa(i%redact.MaxKept)+"#")

				_ = secrets.String("text")
			}
		})
	}

	wg.Wait()

	for w := range 8 {
		for i := range redact.MaxKept {
			value := "v" + strconv.Itoa(w) + "-" + strconv.Itoa(i) + "#"
			assert.Equal(t, "[secret s"+strconv.Itoa(w)+"]", secrets.String(value), "no Add is lost")
		}
	}
}

// TestCutKeepsWhatMayStartAValue holds where a text logged in pieces is cut: before the bytes that may start a value
// the next piece completes, after a whole value the cut would split, and nowhere when nothing is to keep.
func TestCutKeepsWhatMayStartAValue(t *testing.T) {
	t.Parallel()

	var secrets redact.Redactor

	secrets.Add("k", "SECRET") // 6 bytes: a piece keeps its last 5

	tests := []struct {
		name, text string
		want       int
	}{
		{"keeps the last bytes", "0123456789abcdef", 11},
		{"never splits a whole value", "0123456789SECRET", 16},
		{"a value before the kept bytes", "SECRET6789abcdef", 11},
		{"a value straddling the cut", "0123456SECRETdef", 13},
		{"too short to keep anything", "012345678", 9},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, secrets.Cut([]byte(tc.text)))
		})
	}

	var empty redact.Redactor

	assert.Equal(t, 16, empty.Cut([]byte("0123456789abcdef")), "no value: nothing to keep")
}

// TestTrimDropsWhatMayEndAValue holds what the end of a longer text drops: the first bytes, which may end a value
// whose start was cut away, but never part of a whole value.
func TestTrimDropsWhatMayEndAValue(t *testing.T) {
	t.Parallel()

	var secrets redact.Redactor

	secrets.Add("k", "SECRET")

	tests := []struct {
		name, text string
		want       int
	}{
		{"drops the first bytes", "0123456789abcdef", 5},
		{"keeps a whole value at the start", "SECRET6789abcdef", 0},
		{"keeps a value straddling the cut", "012SECRET9abcdef", 3},
		{"too short to drop anything", "012345678", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, secrets.Trim([]byte(tc.text)))
		})
	}
}

// TestALongValueLeavesTheOthersProtected: a value too long for a text to protect does not stop Cut and Trim from
// protecting the values that fit, and Span sizes a text that protects them all.
func TestALongValueLeavesTheOthersProtected(t *testing.T) {
	t.Parallel()

	var secrets redact.Redactor

	secrets.Add("k", "SECRET")
	secrets.Add("cert", strings.Repeat("C", 20)) // longer than half of the 16-byte texts below

	assert.Equal(t, 13, secrets.Cut([]byte("0123456SECRETdef")), "SECRET straddling the cut stays whole")
	assert.Equal(t, 11, secrets.Cut([]byte("0123456789abcdef")), "the last bytes that may start SECRET are kept")
	assert.Equal(t, 3, secrets.Trim([]byte("012SECRET9abcdef")), "SECRET straddling the trim stays whole")
	assert.Equal(t, 5, secrets.Trim([]byte("0123456789abcdef")), "the first bytes that may end SECRET go")

	assert.Equal(t, 40, secrets.Span(16), "twice the longest value")
	assert.Equal(t, 4096, secrets.Span(4096), "at least the size asked for")

	var none *redact.Redactor

	assert.Equal(t, 16, none.Span(16))
}

// TestScrubsWithoutAMatchDoNotAllocate: String runs on every line a guest writes and every request a
// plugin makes, mostly holding no value; with several values held it allocates only when one matches (`make
// allocs`, part of `make check`, runs it under the race detector).
//
//nolint:paralleltest // testing.AllocsPerRun panics in parallel tests
func TestScrubsWithoutAMatchDoNotAllocate(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("allocations are measured without coverage instrumentation, by make allocs")
	}

	var secrets redact.Redactor

	secrets.Add("api_key", "tok-123456")
	secrets.Add("db_password", "hunter2-hunter2")

	text := "GET https://api.example.com/v1/items: 200 OK"
	got := ""

	allocs := testing.AllocsPerRun(100, func() { got = secrets.String(text) })

	assert.Zero(t, allocs)
	assert.Equal(t, text, got)
	assert.Equal(t, "GET [secret api_key]", secrets.String("GET tok-123456"), "a match is still scrubbed")
}

// FuzzPiecesNeverShowAValue: a text cut into pieces where Cut says, each scrubbed, never shows a value whose start
// Cut could keep (a value at most half a piece long), wherever it falls and however long the other value is; and
// pieces as long as Span says never show either value.
func FuzzPiecesNeverShowAValue(f *testing.F) {
	f.Add("0123456789SECRETabcdef0123456789abcdefSECRET", "SECRET", "LONGLONGLONGLONG", uint8(16))
	f.Add("xxxxxxxxxxxxxxxTOKENTOKENTOKEN", "TOKEN", "TOKENTOKENTOKENTOKEN", uint8(9))
	f.Add("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaa", "b", uint8(8))
	f.Add(strings.Repeat("x", 11)+"SECRET"+strings.Repeat("y", 9)+strings.Repeat("Z", 30)+"SECRET",
		"SECRET", strings.Repeat("Z", 30), uint8(16))

	f.Fuzz(func(t *testing.T, text, short, long string, size uint8) {
		values := []string{short, long}
		// A value the mask holds, or that a mask could complete, is out of scope: no value is scrubbed twice.
		for _, v := range values {
			if v == "" || strings.ContainsAny(v, "[secret kl]") {
				t.Skip()
			}
		}

		var secrets redact.Redactor

		secrets.Add("k", short)
		secrets.Add("l", long)

		base := int(size%57) + 8

		for _, piece := range []int{base, secrets.Span(base)} {
			shown := pieces(t, &secrets, text, piece)

			for _, v := range values {
				if len(v)-1 > piece/2 {
					continue // too long for the piece: not protected
				}

				for _, s := range shown {
					require.NotContains(t, s, v, "pieces of %d bytes: %q", piece, shown)
				}
			}
		}
	})
}

// pieces returns text cut into pieces of at most piece bytes where Cut says, each scrubbed.
func pieces(t *testing.T, secrets *redact.Redactor, text string, piece int) []string {
	t.Helper()

	var shown []string

	for buf := []byte(text); ; {
		if len(buf) <= piece {
			return append(shown, secrets.String(string(buf)))
		}

		cut := secrets.Cut(buf[:piece])
		require.Positive(t, cut, "every piece makes progress")

		shown = append(shown, secrets.String(string(buf[:cut])))
		buf = buf[cut:]
	}
}
