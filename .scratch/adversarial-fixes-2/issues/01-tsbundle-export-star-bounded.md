# 01: TypeScript bundling cannot exhaust host CPU

**Priority:** Critical · **Difficulty:** Hard · **Category:** Security, CPU exhaustion

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 1 (CONFIRMED by the coordinator).

**What to build:** Loading a TypeScript plugin can never spend unbounded host CPU in the bundler. Today `Bundle` runs esbuild synchronously on the host, without cancellation, before any wasm limit applies. esbuild re-walks shared `export *` subgraphs, so a plugin of N layers of two modules, each re-exporting both modules of the previous layer, costs about 2^N work. Measured on esbuild v0.28.2: 16 layers 85 ms, 18 layers 393 ms, 20 layers 1.76 s (×≈2.1 per layer); 30 layers ≈ 30 min, 40 layers ≈ a year, from a few dozen tiny files. Register and reload/polling are blocked meanwhile and cancelling their ctx does nothing.

Options (decide and document): bound the import graph before calling esbuild (module count, import edges, `export *` depth/fan-out, computed by a cheap pre-scan of the plugin's files), and/or run bundling in a separate process that ctx cancellation and a CPU/time limit can kill. Returning on timeout while the goroutine keeps running is not acceptable. Normal plugins must keep bundling as today.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A test with the layered `export *` plugin (sized so the fixed version fails fast and the old one exceeds a generous bound) reproduces the bug (fails on the current code) and passes with the fix
Register/reload of such a plugin fails promptly with a clear error, and cancelling the ctx stops bundling (no goroutine left running: goleak)
Existing TS plugins (multi-file, JSON/text imports) bundle unchanged; limits documented in README and PLAN.md
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
