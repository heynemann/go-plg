// Package helpertest sits in a directory ending in _test: plg build-guest must not name its extracted symbol
// file after it, or the go command would skip the file as a test file.
package helpertest

// Suffix is in example.com/greet/helper_test.
func Suffix() string { return "test" }
