// Command plg is plg's command-line tool. In a contract package, next to the interfaces:
//
//	//go:generate go run github.com/heynemann/go-plg/cmd/plg gen Fooer
//
// generates the adapter plg calls plugins through, and registers it with the package's source. See
// [github.com/heynemann/go-plg.Contract]. `plg warm` fills the compilation cache with the guests plg ships (see
// [github.com/heynemann/go-plg.Warm]), and `plg build-guest` builds a custom Go guest with extra packages (see
// [github.com/heynemann/go-plg.WithGoGuest]); `plg` without arguments prints the usage.
package main

import (
	"os"

	"github.com/heynemann/go-plg/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], ".", os.Stderr))
}
