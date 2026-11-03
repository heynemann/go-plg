# 08: Skipped json:"-" fields cannot bypass the allocation guard

**Severity:** medium (from the code review of `a1988b6`)

**What to build:** Decoding a slice or map from untrusted guest bytes never allocates far more memory than the input justifies. Today fields tagged `json:"-"` contribute nothing to an element's minimum encoded size, so an element type like `` struct{ OK bool; Cache Big `json:"-"` } `` (Big ≈ 8 KiB) has a minimum size of 1 byte: a result of `count=N` followed by N zero bytes makes the decoder allocate N × 8 KiB — a 1 MiB body asks for 8 GiB, and Go's out-of-memory is fatal (no `recover` can catch it). Bound decoded memory by bytes (e.g. count × element size against a multiple of remaining input) or reject element types whose in-memory size to encoded-minimum ratio is too large at contract registration, with the same rule in the generated fast paths and their layout.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A regression test feeding the review's crafted body to both the generic and the generated decoder reproduces the bug (fails on the current code) and passes with the fix
- [ ] Both decoders reject the input with a conversion/ABI error before allocating; memory stays bounded (assert with allocation measurement)
- [ ] Fuzz targets extended with such element types; the generic and generated paths still agree (existing equivalence fuzz passes)
- [ ] Legitimate large results within limits still decode
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
