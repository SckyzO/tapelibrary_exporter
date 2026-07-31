# samples/

Raw material gathered from whatever `tapelibrary_exporter` monitors: captured
output (HTTP responses, command output) and the target's own API
documentation (an OpenAPI or gRPC specification, exported doc pages).

Everything in this directory is **ignored by git**, except this file. That is
deliberate, for two independent reasons:

- **It is not anonymized.** Captured output routinely carries real hostnames,
  tenant names, account names, and sometimes credentials. `CONTRIBUTING.md`
  requires every test fixture to be anonymized before it is committed, and
  forbids committing output copy-pasted from a production system.
- **It may not be yours to redistribute.** A vendor's API documentation
  carries its own terms, which this repository's license does not decide.

This file is tracked so the directory itself survives a clone. Git does not
track empty directories.

## How it is used

Material here is **derived from, never moved out of**. A collector's fixture
under `internal/collector/testdata/` is a *trimmed and anonymized* copy of
something here; the original stays, because one capture commonly covers
several resources and because a later collector should not have to go back to
the live target to get it again.

```
samples/pools-list.json          raw, NOT anonymized, stays here
        |
        v  trim to the parsed shape, anonymize per CONTRIBUTING.md
internal/collector/testdata/pools.json    committed
```

## What does not go here

The exporter's own documentation. `docs/` is written for the people who run
this exporter; a vendor's documentation is working material, not a
deliverable.
