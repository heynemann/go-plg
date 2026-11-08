// Package offline is how `plg validate` loads a plugin with plg's own loading code, without the service that
// compiles the contract in: the command describes the contract from its source ([Contract]) and package plg, which
// sets [Check] when it is linked, loads the plugin against it as plg.Check loads a candidate plugin version.
package offline

import (
	"context"
	"io/fs"
	"log/slog"
	"reflect"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/goabi"
)

// Contract is a contract as plg validate resolved it: the interface a plugin implements, described from the
// contract's source.
type Contract struct {
	// Interface is the interface as the Go dispatcher writes it.
	Interface *goabi.Interface
	// Host is the Host the plugin's Create takes; nil when it takes none.
	Host *Host
	// Source holds the Go source files of the interface's package at its root.
	Source fs.FS
	// Module is the contract module plg validate resolved at the plugin's pinned version.
	Module contractref.Module
	// Methods are the interface's methods, in Interface's order, of the types the Go interpreter gives the
	// contract's types: the types plg plans the script guests' schema from.
	Methods []reflect.Method
}

// Host is a Host interface of the contract. plg validate runs without the service's Host: every call of one of its
// methods fails.
type Host struct {
	// Interface is the Host as the Go dispatcher writes it.
	Interface *goabi.Interface
	// Methods are the Host's methods, as Contract's.
	Methods []reflect.Method
}

// Plugin is the plugin plg validate checks: its directory.
type Plugin struct {
	// Files are the plugin's files, plugin.yaml among them.
	Files fs.FS
	// Key is the plugin's key: its directory's name, as a storage of plugin directories keys it.
	Key string
	// Manifest is the content of its plugin.yaml.
	Manifest []byte
}

// Options configure [Check].
type Options struct {
	// Logger gets what the plugin writes and what plg logs while loading it.
	Logger *slog.Logger
	// CacheDir is the guests' compilation cache directory; empty means plg's default.
	CacheDir string
}

// Check loads plugin against contract and runs its Create, with the stub Host when contract has one, in a throwaway
// instance, as plg.Check runs a candidate. It returns the load's report, whose Error is why the plugin would not
// load; it fails when the manifest is invalid, the contract is not one plg can serve, or the check could not run.
// Package plg sets it.
//
//nolint:gochecknoglobals // the one seam from the command to plg's unexported loading code, set from plg's init
var Check func(ctx context.Context, contract *Contract, plugin *Plugin, opts *Options) (*adminjson.Report, error)
