// Package secretabi connects [github.com/heynemann/go-plg/plugin.Secret] to the Go guest running the plugin: the
// guest's reactor sets [Read] before any plugin code runs. Outside a guest it stays nil, and plugin.Secret fails.
package secretabi

// Read returns the value of the plugin's secret name, or why the host refused it. The Go guest sets it once, while
// its single thread initializes; nothing else writes it.
//
//nolint:gochecknoglobals // the one link from the compiled plugin package to the guest's host import
var Read func(name string) (string, error)
