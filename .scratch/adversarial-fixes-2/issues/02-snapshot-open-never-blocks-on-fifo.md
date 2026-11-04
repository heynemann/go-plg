# 02: Snapshot and manifest opens never block on a swapped-in FIFO

**Priority:** High · **Difficulty:** Moderate · **Category:** Security, TOCTOU

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 5 (completes adversarial fix 05 and review fix 09).

Inferred from reading the code: reproduce before fixing.

**What to build:** Copying a plugin's files or reading `plugin.json` can never block forever. Today a file that the walk saw as regular can be swapped for a FIFO before `parent.Open(part)`; a read-only open of a FIFO blocks until a writer appears, so no type/identity check or ctx cancellation runs and Register or a reload round stalls. `readManifest` has the same race between its stat and open. Open without blocking (e.g. `O_NONBLOCK` through the rooted open, then require a regular file and the identity checks before reading, clearing non-blocking mode if needed); remove the "FIFO swap" residual-risk note from PLAN.md/README once fixed.

**Blocked by:** None (can start immediately)

**Status:** done

**Resolution:** `NewFSStorage` never opens without `O_NONBLOCK`: files and directories through the walk, the copy and List open at once on a FIFO, and the existing checks refuse it (`replaced while the plugin's files were copied`, `a plugin key is a directory name`, `plugin.json is not a regular file`). Directories are opened as `name/.`, so `os.Root` opens them as directories only. fs.FS trees cannot open without waiting; that is documented. See PLAN.md, "Second adversarial review fixes", 02.

- [x] Deterministic tests swapping a plugin file and `plugin.json` for a FIFO between the walk/stat and the open (via the existing test hooks) reproduces the bug (fails on the current code) and passes with the fix
- [x] The load fails promptly with the existing 'replaced' / invalid-manifest error; no open blocks
- [x] Docs and PLAN.md residual-risk notes updated
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
