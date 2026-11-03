# 02: Plugin source snapshots are bounded

**Priority:** Critical · **Difficulty:** Moderate · **Category:** Security, Unfulfilled promise

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** Copying a plugin's files into memory can never exhaust the host. Today `snapshotFiles` (and the built-in FS storage's copy) read every regular file in full with `fs.ReadFile`, with no per-file or total cap and no ctx checks — including files the language loader never uses. A large (e.g. sparse) file next to a tiny valid plugin makes the host allocate its advertised size; Go's out-of-memory is fatal, and the container memory budget does not cover source snapshots.

Enforce a documented per-file and per-plugin total size limit (shared constant(s), exported if storages need them), checked before allocating and enforced during the read (files can grow while being read), and honour ctx cancellation while copying. Applies to built-in and custom storages. Note ticket 05 touches the same snapshot code.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] Tests with a file over the per-file cap and a set of files over the total cap (sparse files, so they cost no disk) reproduces the bug (fails on the current code) and passes with the fix
- [x] Loads fail promptly with a clear error naming the plugin and file; memory stays bounded (asserted via allocation measurement)
- [x] A file that grows during reading cannot exceed the cap
- [x] Cancelling Register's ctx stops a long copy
- [x] Limits documented in README (Storages) and on `Storage`/`FSStorage`; PLAN.md log updated
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket

Resolution: `plg.MaxPluginFileSize` (16 MiB) and `plg.MaxPluginSize` (64 MiB) bound every snapshot, with `plg.ErrPluginTooLarge`; stated sizes are checked before reading, reads stop one byte past what is left, and ctx is checked per entry and per read. `TestPluginFilesOverTheLimitsAreNeverRead` (16.8 MB / 67 MB allocated, no error → `ErrPluginTooLarge`, 15 KB / 4 KB), `TestGrowingPluginFilesStopAtTheLimits`, `TestCancellingStopsTheCopyOfPluginFiles` and `TestCancellingRegisterStopsTheCopyOfPluginFiles` failed before the fix and pass. Details in PLAN.md (Adversarial review fixes, 02).
