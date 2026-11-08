// Package secretabi connects [github.com/heynemann/go-plg/plugin.Secret] to the Go guest running the plugin: the
// guest's reactor sets [Read] before any plugin code runs. Outside a guest it fails every read with [ErrNoGuest].
package secretabi

import "errors"

// ErrNoGuest is what [Read] fails with outside a plg guest; plugin.ErrNoGuest is the same error.
var ErrNoGuest = errors.New("plugin: secrets are read only inside plg")

// Read returns the value of the plugin's secret name, or why the host refused it. The Go guest sets it once, while
// its single thread initializes; nothing else writes it. Until then it fails with [ErrNoGuest].
//
//nolint:gochecknoglobals // the one link from the compiled plugin package to the guest's host import
var Read = noGuest

// noGuest is Read outside a guest.
func noGuest(string) (string, error) {
	return "", ErrNoGuest
}
