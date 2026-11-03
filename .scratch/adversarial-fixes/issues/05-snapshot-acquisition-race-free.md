# 05: Snapshot acquisition never follows a replaced symlink

**Priority:** High · **Difficulty:** Moderate · **Category:** Security

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** Completing review fix 04: copying a plugin's files cannot follow a symlink swapped in after the directory walk. Today the snapshot trusts the walk's regular-file entry and then reads the path again by name, so replacing `p/x.txt` with a link to `../secret.txt` between the two copies a file from elsewhere in the storage root (or anywhere on the host for `os.DirFS`-backed storages), and a TS plugin can import and expose it.

Open and read through race-safe rooted operations that never follow symlinks (e.g. `os.Root` with no-follow semantics / verifying the opened file is regular and matches the entry, intermediate directories included) for the built-in storages; for arbitrary custom `fs.FS`, document what plg can and cannot guarantee. Coordinate with ticket 02 (same code).

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A deterministic test that swaps a file for an escaping symlink between walk and read (via an injected fs hook) reproduces the bug (fails on the current code) and passes with the fix
- [x] The swapped file is refused with the existing symlink error; no outside content is copied
- [x] Intermediate-directory swaps are covered too
- [x] FSStorage/Storage docs and PLAN.md updated with the guarantee for built-in vs custom storages
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket

Resolution: `NewFSStorage`'s Open copies through `snapshotDir`: the plugin directory is opened once as an `os.Root` before the walk, then every directory and file is opened relative to its open parent and must be the very entry (`os.SameFile` with the parent's `Lstat`) of the kind the walk listed; an entry that became a link is judged as the walk judges links (left out inside the plugin, `<path>: symbolic link out of the plugin's files (to <target>)` otherwise), one replaced otherwise fails with `replaced while the plugin's files were copied`. `TestFSStorageCopyNeverFollowsASwappedLink` (test hook `OpenSwapping`): file, directory and plugin-directory swaps copied the storage's secret (or another file's contents) before the fix and are refused / left out after. Custom fs.FS trees can only be trusted as far as the fs.FS goes: documented on `Storage`, `FSStorage`, `NewFSStorageFromFS` and README. Details in PLAN.md (Adversarial review fixes, 05).
