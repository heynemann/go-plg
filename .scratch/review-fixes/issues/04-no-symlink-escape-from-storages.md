# 04: Plugins cannot read host files through symlinks

**Severity:** high (from the code review of `a1988b6`)

**What to build:** Built-in storages never follow a symlink out of the plugin's directory. Today the filesystem storage uses `os.DirFS`, which follows symlinks, so a TS plugin directory containing `x.txt -> /proc/self/environ` can `import` it and exfiltrate host secrets (via return values, logs or fetch), and a Go manifest whose entrypoint is a symlink to `/` makes the loader walk and copy the host's whole tree into memory and mount it in the guest. This contradicts the documented guarantees (FSStorage: symlinked files are skipped; the TS bundler never touches the host file system).

Approach suggested by the review (pick and document): open storages with `os.OpenRoot` (Go ≥1.24 rooted FS that refuses escapes), and/or snapshot each opened plugin once into the in-memory overlay (which already skips irregular files) and hash, bundle and load only from that snapshot, so every language reads the same safe copy.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] Regression tests (TS import of a symlink to a host file; Go entrypoint symlinked to `/`; Lua `require` through a symlink) reproduces the bug (fails on the current code) and passes with the fix
- [ ] Each case fails the plugin's load with a clear error naming the offending path; no host file content reaches the guest
- [ ] Symlinks that stay inside the plugin directory have documented behaviour (followed or skipped) and a test
- [ ] Version hashing, bundling and loading all read the same snapshot (no TOCTOU between hash and load)
- [ ] README/FSStorage docs updated to match
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
