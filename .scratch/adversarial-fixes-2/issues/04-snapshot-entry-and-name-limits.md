# 04: Plugin snapshots bound entry counts and path sizes

**Priority:** High · **Difficulty:** Moderate · **Category:** Security, memory exhaustion

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 3 (completes adversarial fix 02).

Inferred from reading the code: reproduce before fixing.

**What to build:** Copying a plugin's files can never exhaust host memory through many entries or long names. Today the snapshot limits count only file contents: arbitrarily many empty files or directories, or very long paths, are listed and built into the in-memory tree outside any budget, and `fs.WalkDir` reads a whole directory's entries before any callback can stop it. Add documented limits on entry count and total path bytes, enforced while directories are read (bounded `ReadDir(n)` batches for the rooted copy and for `fs.FS` storages that support `fs.ReadDirFile`), with the S3 example's pagination respecting them.

**Blocked by:** None (can start immediately)

**Status:** done

**Resolution:** `plg.MaxPluginEntries` (16,384 directory entries of any kind) and `plg.MaxPluginPathBytes` (2 MiB of entry paths together), next to `MaxPluginSize`, bound the listing: directories are read through `fs.ReadDirFile` in batches charged as they are read, so the read that brings a plugin over is its last (an fs.FS whose directories are not one is read a directory at a time through `fs.ReadDirFS`, documented); the S3 example's List and Open stop paging at the page that brings a plugin over. See PLAN.md, "Second adversarial review fixes", 04.

- [x] Tests with a plugin containing too many empty files, too many empty directories, and an over-long total of path bytes reproduces the bug (fails on the current code) and passes with the fix
Each fails with `ErrPluginTooLarge` naming the plugin before the listing is fully read (allocation measured)
Limits exported/documented next to `MaxPluginSize`; S3 example honours them
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
