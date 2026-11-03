# 15: Examples and README

**What to build:** A newcomer can adopt plg from the docs alone: a README covering concepts, the quick start, the interface rules, storages, the go:generate step, limits, HTTP, stability guarantees and residual risks. Examples: an S3 storage (aws-sdk-v2), a Postgres storage listing only enabled plugins (pgx), and a small service with Go, Lua and TS plugins for one interface. Examples live outside the core package's dependencies.

**Blocked by:** 05 (Filesystem and fs.FS storages), 12 (TypeScript plugins), 13 (Lua plugins)

**Status:** done

- [x] Every example builds and has a smoke test (S3 and Postgres runnable against local containers)
- [x] The core module's dependency list stays free of aws-sdk and pgx
- [x] README quick start is copy-paste runnable
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
