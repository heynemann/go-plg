# 11: Tracing

**What to build:** Teams can follow one request across platform code and plugins. `plg.WithTracerProvider(trace.TracerProvider)` (OTel trace API only; global provider by default) creates a span per plugin call named `plg.call <key>.<method>` with key, version, language, method, outcome and owner team/org/company attributes, child spans for Host method calls and HTTP requests, `traceparent` on plugin HTTP requests, and `trace_id`/`span_id` on guest log records. The caller's ctx provides the parent span. No extra allocations when tracing is a no-op provider.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] Tests with the SDK's in-memory exporter: spans and attributes for a call, a Host call and an HTTP request (traceparent received by the test server), parent/child relationships, error status on failures, guest log records carrying the IDs
- [x] Benchmarks show no added allocations on calls with the no-op provider
- [x] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket (CI run 42451722767 for `6e3a3af`: success)
