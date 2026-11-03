//go:build wasip1

// Command golang is the generic Go guest: yaegi compiled to wasip1 as a reactor module exporting the plg guest
// ABI (package reactor), with the symbol tables of the standard library allowlist (package symbols).
//
// Build it with `make guests`.
package main

import _ "github.com/heynemann/go-plg/guestsrc/golang/reactor" // the guest ABI exports

func main() {}
