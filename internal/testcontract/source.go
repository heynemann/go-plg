package testcontract

import "embed"

// Source is the contract package's source as the guest interprets it.
//
//go:embed contract.go
var Source embed.FS
