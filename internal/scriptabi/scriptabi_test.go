package scriptabi_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/scriptabi"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestPayloadLayout pins the bytes the C glue of the script guests parses.
func TestPayloadLayout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		want    []byte
		payload scriptabi.Payload
	}{
		{
			name:    "every part",
			payload: scriptabi.Payload{Name: "a.js", Source: "x()", Methods: 2, Schema: []byte{9, 8}},
			want: []byte{
				4, 0, 0, 0, 'a', '.', 'j', 's',
				3, 0, 0, 0, 'x', '(', ')',
				2, 0, 0, 0,
				9, 8,
			},
		},
		{
			name:    "empty source, no methods",
			payload: scriptabi.Payload{Name: "m.lua", Schema: []byte{0, 0, 0, 0, 0, 0, 0, 0}},
			want: []byte{
				5, 0, 0, 0, 'm', '.', 'l', 'u', 'a',
				0, 0, 0, 0,
				0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tc.payload.Append(nil))
			assert.Equal(t, append([]byte("prefix"), tc.want...), tc.payload.Append([]byte("prefix")), "appends")
		})
	}
}
