# T-14 — Genericity proof: the poker compatibility probe

**dcrpoker's entire suite passes against the evolved SDK. 9/9 packages, both
consumer goldens green, nothing deployed.**

Branch `runtime-compat-probe` (dcrpoker) · 2026-08-19

---

## What was proved, and what was not

The ticket asks for two things. One is done and one is not, and they are worth
keeping apart.

**Done: the SDK's evolution did not break its live consumer.** dcrpoker is
pinned at v0.2.0 and holds real coin. Everything the runtime work added -
appending bond terms to `Terms.Hash`, changing `AwaitSpend`, moving `punish` and
`evidence` in - had to leave poker's bytes and behaviour untouched, and it did.

**Not done: poker does not yet run *on* the runtime.** Its seating and
settlement call sites have not been ported to `Rules`, because the funding stage
does not exist to port them onto. The interface-shape half of the proof is
T-05's, where both games' shapes were implemented against `Rules` as a
compile-time fact.

## Method

A throwaway branch with one change:

```
go mod edit -replace github.com/karamble/dcrgaming-sdk=<local checkout>
```

Nothing else. `git diff sdk runtime-compat-probe` outside `go.mod`/`go.sum` is
empty, so poker's own code is bit-for-bit what it was.

## Result

```
ok  github.com/vctt94/dcrpoker/cmd/dcrpoker            159.372s
ok  github.com/vctt94/dcrpoker/internal/config           0.114s
ok  github.com/vctt94/dcrpoker/internal/log              0.040s
ok  github.com/vctt94/dcrpoker/pkg/chainwatch            0.175s
ok  github.com/vctt94/dcrpoker/pkg/deck                 68.120s
ok  github.com/vctt94/dcrpoker/pkg/driver              101.113s
ok  github.com/vctt94/dcrpoker/pkg/gaming/cardschema     6.928s
ok  github.com/vctt94/dcrpoker/pkg/poker                 0.039s
ok  github.com/vctt94/dcrpoker/pkg/replay               11.722s
```

`go build ./...` and `go vet ./...` also clean.

## The two that mattered most

`cmd/dcrpoker/consumer_golden_test.go` holds the pins that run under poker's own
module graph, and they are the ones this whole probe existed for:

- **`TestTheConsumedTermsHashIsPinned`** - the digest every join and commit at a
  live table binds to. T-03 added bond terms to `membership.Terms`. Had they been
  hashed unconditionally rather than appended only when present, this is where it
  would have shown, and every signature at every live poker table would have
  stopped verifying.
- **`TestTheConsumedEscrowScriptIsPinned`** - the script bytes guarding live
  bonds.

Both green.

## Deployment

Not done, and deliberately. The probe branch carries a `replace` to a local
checkout and must never reach a daemon. Whether poker's live daemons ever adopt
the runtime is a separate maintainer decision, to be taken after the funding
stage lands and the acid test is complete.

`sdk` - poker's real branch - is untouched.
