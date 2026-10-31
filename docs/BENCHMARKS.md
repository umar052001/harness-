# Model evaluations

Run from `go/` after building sandboxd:

```sh
go run ./cmd/bench -model opencode/big-pickle -repeat 3 > ../benchmark-results.jsonl
```

These are live agent runs and can consume provider credits. They are deliberately
excluded from `make test`. Use `-case data-summary` or `-case normalize-tags` for a
single task, and `-timeout 10m` to set a per-attempt limit. Attempts run sequentially
in fresh temporary workspaces, removed after verification. Avoid running benchmarks
alongside dashboard builds when measuring latency.

Requirements: an installed OpenCode, sandboxd, and `/usr/bin/python3` for the tag
normalization verifier. Generated code is verified inside a second sandbox without
network access. Verification logic is supplied by the host after the agent exits.

## Cases

- `data-summary`: produce exact integer totals from a CSV fixture, including a
  zero-quantity region. The verifier checks the output independently.
- `normalize-tags`: repair a Python function. Checks cover order, normalization,
  deduplication, empty input, invalid types, and input mutation.

The JSONL report includes case version, model, timestamp, elapsed time, verified
outcome, and reported usage. It excludes prompts, transcripts, and source files.
Unknown accounting remains absent. Report files are gitignored.

## Comparing results

Use the same case version, model settings, machine, and attempt count. Report the
verification pass rate alongside median duration and token/cost distributions.
Include failed attempts in spend, and identify missing usage rather than treating
it as zero. Do not compare costs across currencies without an explicit conversion.

These cases establish a narrow baseline for the current unassisted agent workflow.
They do not measure UI quality, full-stack reliability, or scalability. They do not
yet establish savings over standalone OpenCode. A standalone comparison must use
the same fixtures, instructions, model, and acceptance checks; future capability
packs must be compared against this baseline before claiming improvements.

## Full-stack smoke check — 2026-10-26

A fresh Big Pickle run generated an inventory application with separate web/API
services, managed PostgreSQL, and an acceptance script using the versioned runtime
contract. Agent prompt time was 323.4 seconds; reported usage was 940,597 total
tokens, including 862,720 cache-read tokens, with a USD 0 agent cost estimate.
Categories may overlap; this is not independently reconciled billing.

The first runtime build exposed a missing resolver grant during npm installation.
After fixing shared sandbox DNS handling, the same generated application passed
its acceptance script without application-code edits. Browser checks covered
create/update/delete, invalid input, mobile overflow, and data retained across an
app restart. All test records were removed. One embedded interaction stalled once
after restart; a fresh load and a subsequent restart/delete test passed, so its
cause was not established.

This is an end-to-end smoke result, not evidence that arbitrary applications always
work or that this approach costs less than standalone OpenCode. The high reported
token total is a baseline for future template/context optimizations, not a savings
claim. Generated projects and raw transcripts remain outside the repository.
