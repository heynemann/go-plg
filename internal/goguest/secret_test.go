package goguest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/goguest"
	"github.com/heynemann/go-plg/internal/wire"
)

func TestSecretAnswerDecodesTheHostsAnswer(t *testing.T) {
	t.Parallel()

	value, err := goguest.SecretAnswer(msgAnswer(wire.StatusOK, "s3cr3t"))
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t", value)

	value, err = goguest.SecretAnswer(msgAnswer(wire.StatusOK, ""))
	require.NoError(t, err)
	assert.Empty(t, value, "an empty value is a value")

	_, err = goguest.SecretAnswer(msgAnswer(wire.StatusError, `plg: secret "x" is not declared`))
	require.EqualError(t, err, `plg: secret "x" is not declared`, "the host's reason, verbatim")
}

func TestSecretAnswerRefusesMalformedAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		msg    string
		answer []byte
	}{
		{name: "empty", answer: nil, msg: "plg: malformed answer to a secret read"},
		{name: "no message", answer: []byte{byte(wire.StatusOK)}, msg: "plg: malformed answer to a secret read: "},
		{
			name: "trailing bytes", answer: append(msgAnswer(wire.StatusOK, "v"), 0),
			msg: "plg: malformed answer to a secret read: ",
		},
		{
			name: "unknown status", answer: msgAnswer(wire.Status(9), "v"),
			msg: "plg: malformed answer to a secret read: status 9",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			value, err := goguest.SecretAnswer(tc.answer)
			require.ErrorContains(t, err, tc.msg)
			assert.Empty(t, value)
		})
	}
}
