package goguest

import (
	"errors"
	"fmt"

	"github.com/heynemann/go-plg/internal/wire"
)

// errSecretAnswer reports an answer to a secret read that is not one the host gives.
var errSecretAnswer = errors.New("plg: malformed answer to a secret read")

// SecretAnswer decodes the host's status-prefixed answer to a secret read (the plg.secret import): the value for
// [wire.StatusOK], otherwise the host's reason as an error.
func SecretAnswer(answer []byte) (string, error) {
	if len(answer) == 0 {
		return "", errSecretAnswer
	}

	reader := wire.NewReader(answer[1:])

	text, err := reader.Text()
	if err == nil {
		err = reader.End()
	}

	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %w", errSecretAnswer, err)
	case wire.Status(answer[0]) == wire.StatusOK:
		return text, nil
	case wire.Status(answer[0]) == wire.StatusError:
		return "", errors.New(text) //nolint:err113 // only the host's reason crosses the boundary
	default:
		return "", fmt.Errorf("%w: status %d", errSecretAnswer, answer[0])
	}
}
