package offline_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/heynemann/go-plg/internal/offline"
)

// TestParamNameNamesByIndex: an unnamed parameter is named p<i>, as plg gen names it, so test cases give it by
// that name.
func TestParamNameNamesByIndex(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "p1", offline.ParamName(1))
	assert.Equal(t, "p12", offline.ParamName(12))
}
