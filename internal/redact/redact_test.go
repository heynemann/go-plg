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
	secrets.Add("again", "s3cret")      // recorded already, under api_key

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

// FuzzPiecesNeverShowAValue: a text cut into pieces where Cut says, each scrubbed, never shows a value whose start
// Cut could keep (a value at most half a piece long), wherever it falls.
func FuzzPiecesNeverShowAValue(f *testing.F) {
	f.Add("0123456789SECRETabcdef0123456789abcdefSECRET", "SECRET", uint8(16))
	f.Add("xxxxxxxxxxxxxxxTOKENTOKENTOKEN", "TOKEN", uint8(9))
	f.Add("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaa", uint8(8))

	f.Fuzz(func(t *testing.T, text, value string, size uint8) {
		piece := int(size%57) + 8
		// A value the mask holds, or that a mask could complete, is out of scope: no value is scrubbed twice.
		if value == "" || len(value)-1 > piece/2 || strings.ContainsAny(value, "[secret k]") {
			t.Skip()
		}

		var secrets redact.Redactor

		secrets.Add("k", value)

		var shown []string

		for buf := []byte(text); ; {
			if len(buf) <= piece {
				shown = append(shown, secrets.String(string(buf)))

				break
			}

			cut := secrets.Cut(buf[:piece])
			require.Positive(t, cut, "every piece makes progress")

			shown = append(shown, secrets.String(string(buf[:cut])))
			buf = buf[cut:]
		}

		for _, s := range shown {
			require.NotContains(t, s, value, "pieces %q", shown)
		}
	})
}
