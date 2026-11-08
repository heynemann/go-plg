// Command plg is plg's command-line tool. In a contract package, next to the interfaces:
//
//	//go:generate go run github.com/heynemann/go-plg/cmd/plg gen Fooer
//
// generates the adapter plg calls plugins through, and registers it with the package's source, checking the
// interfaces' plg directives (//plg:limits, //plg:guest-packages, //plg:validation). See
// [github.com/heynemann/go-plg.Contract]. `plg warm` fills the compilation cache with the guests plg ships (see
// [github.com/heynemann/go-plg.Warm]), `plg build-guest` builds a custom Go guest with extra packages (see
// [github.com/heynemann/go-plg.WithGoGuest]), and `plg status` and `plg events` show an environment's plugins
// and their events through its admin handler (see [github.com/heynemann/go-plg.AdminHandler]), named by a
// profile of ~/.config/plg/profiles.yaml, and `plg validate` checks a plugin directory before it is registered:
// it resolves the contract module the plugin pins with the go command and loads the plugin in the real guest,
// under the contract's //plg:limits and, for a Go plugin of a contract with //plg:guest-packages, in that custom
// guest, built and cached;
// `plg` without arguments prints the usage.
package main

import (
	"os"

	"github.com/heynemann/go-plg/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], &cli.Env{
		Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Environ: os.Environ, Dir: ".",
	}))
}
