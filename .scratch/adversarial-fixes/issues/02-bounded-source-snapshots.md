# 02: Plugin source snapshots are bounded

**Priority:** Critical · **Difficulty:** Moderate · **Category:** Security, Unfulfilled promise

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** Copying a plugin's files into memory can never exhaust the host. Today `snapshotFiles` (and the built-in FS storage's copy) read every regular file in full with `fs.ReadFile`, with no per-file or total cap and no ctx checks — including files the language loader never uses. A large (e.g. sparse) file next to a tiny valid plugin makes the host allocate its advertised size; Go's out-of-memory is fatal, and the container memory budget does not cover source snapshots.

Enforce a documented per-file and per-plugin total size limit (shared constant(s), exported if storages need them), checked before allocating and enforced during the read (files can grow while being read), and honour ctx cancellation while copying. Applies to built-in and custom storages. Note ticket 05 touches the same snapshot code.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] Tests with a file over the per-file cap and a set of files over the total cap (sparse files, so they cost no disk) reproduces the bug (fails on the current code) and passes with the fix
Loads fail promptly with a clear error naming the plugin and file; memory stays bounded (asserted via allocation measurement)
A file that grows during reading cannot exceed the cap
Cancelling Register's ctx stops a long copy
Limits documented in README (Storages) and on `Storage`/`FSStorage`; PLAN.md log updated
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
