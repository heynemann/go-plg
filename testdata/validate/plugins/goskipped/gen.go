//go:build ignore
// +build ignore

// gen.go is a generator the `// +build ignore` line keeps out of the Go guest.
package main

import "os"

func main() {
	go os.Exit(0)
}
