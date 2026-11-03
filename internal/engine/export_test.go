package engine

import "runtime/debug"

// PagesFor exposes limitPages to engine_test.
func PagesFor(limit uint64) uint32 { return limitPages(limit) }

// VersionOf exposes depVersion to engine_test.
func VersionOf(deps []*debug.Module, path string) string { return depVersion(deps, path) }
