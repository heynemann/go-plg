//go:build ignore

// Command gzipwasm compresses a guest artifact deterministically: compress/gzip at a fixed level with an empty
// header (no name, zero modification time), so the same toolchain always writes the same bytes and CI can diff
// the committed artifact against a rebuild (`make verify-guests`).
//
// Usage: go run scripts/gzipwasm.go <in.wasm> <out.wasm.gz>
package main

import (
	"compress/gzip"
	"errors"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fail(errors.New("usage: go run scripts/gzipwasm.go <in.wasm> <out.wasm.gz>"))
	}

	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(err)
	}

	out, err := os.Create(os.Args[2])
	if err != nil {
		fail(err)
	}

	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		fail(err)
	}

	_, err = zw.Write(raw)
	if err == nil {
		err = zw.Close()
	}

	err = errors.Join(err, out.Close())
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gzipwasm:", err)
	os.Exit(1)
}
