// Package helperlinux sits in a directory ending in _linux: plg build-guest must not name its extracted symbol
// file after it, or the wasip1 build would skip the file through its GOOS constraint.
package helperlinux

// Suffix is in example.com/greet/helper_linux.
func Suffix() string { return "linux" }
