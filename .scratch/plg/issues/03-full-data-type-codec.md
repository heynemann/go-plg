# 03: Full data-type encoding

**What to build:** Every data type the interface rules allow round-trips between host and Go plugin: structs, struct pointers, slices, string-keyed maps, `[]byte`, `time.Time`, nested combinations and errors. Inside the guest, decoding works on yaegi-interpreted types via reflection. On the host, encoders and decoders are built once per type at Register and cached. Integers that Lua/JS numbers can't represent exactly (|n| > 2^53) fail with an error instead of losing precision. Struct fields use the `json` tag name, otherwise the Go field name (this matters to Lua/TS later).

**Blocked by:** 02 (Tracer bullet)

**Status:** ready-for-agent

- [ ] Round-trip tests through a Go plugin cover every allowed type, including nil pointers, empty vs nil slices/maps, and nested structs
- [ ] Out-of-range integers fail with a conversion error naming the field
- [ ] Encoders are built once per type (verified by benchmark: no per-call plan construction)
- [ ] Benchmarks record ns/op and allocs for a representative struct call
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
