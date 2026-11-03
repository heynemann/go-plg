// Package greet stands for a pure-Go third-party package a custom Go guest adds (plg build-guest -pkg).
package greet

// Hello greets name.
func Hello(name string) string {
	return "hello, " + name
}
