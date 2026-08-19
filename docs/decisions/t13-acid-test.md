# T-13 — The acid test: first measurement

**Partial. 2,056 lines left battleships and it still passes.** The full rebuild
waits on the funding stage.

Branch `runtime-sdk-punish` (dcrbattleships) · 2026-08-19

---

## What was measured

The PRD's acid test is: rebuild the battleships daemon on the runtime, and
whatever a developer still has to hand-write is the SDK's remaining gap.

The runtime is not far enough along for a full rebuild - funding does not exist,
so the daemon still owns its own deposit records - but one whole subsystem could
move now, and moving it is a real measurement rather than a rehearsal.

## Result

`pkg/punish` and `pkg/evidence` are gone from dcrbattleships. It consumes the
SDK's instead.

- **2,067 lines deleted, 10 added**, across 16 files.
- `go build ./...`, `go vet ./...`, `go test ./...` all exit 0; 6/6 packages pass.
- No transaction bytes changed: the shape tests that moved with the code
  (`TestPunishmentSpendsFitTheBridgeShapes`, `TestForfeitSpendNeverCarriesTwoSigs`,
  `TestCleanReleaseComesHome`, `TestWithheldCoSignFallsToTheBackstop`,
  `TestDivergentCellAttestationsSweepTheBond`, `TestLadderRunsAtTwoSeats`) pass
  unchanged against the SDK copy, and battleships' remaining suite passes against
  it in situ.

## What stayed, and why it is the right line

Three things did not move, and each is the boundary the PRD drew:

- **`ShouldAccuse`** is now `match.ShouldAccuse`. Deciding a seat has gone silent
  means knowing what it owed, and what a seat owes is battleships' schedule. The
  SDK carries out a forfeiture; it never rules one.
- **The money matrix** is now `match.Realises`, keyed by the SDK's mechanism
  types. The SDK builds the transaction that takes a bond; what that *means* for
  a game's accounting is the game's. Another game taking the same bond the same
  way may call the cell something else.
- **The fee** became a parameter. `LadderDepth(bond, fee)` and
  `AttritionBound(fee)` used to read battleships' `AccuseFeeAtoms` directly. It
  is an economic choice and the whole attrition bound moves with it.

## One check that was lost, deliberately

`BuildLadder` used to refuse any fee that was not exactly battleships' gv1
number, because the attrition bound assumed it. The SDK cannot hold that
opinion - it is one game's economics - so it now refuses only a fee that is
non-positive or that the bond cannot afford.

The guarantee is not weaker where it matters: `AttritionBound` takes the same
fee the draft carries, so a caller that changes one changes both. But a game
that wants the old "exactly this fee" invariant has to assert it itself, and
battleships should.

## Still to measure

The residual after a *full* rebuild. Today the daemon is still 10,760 non-test
lines and owns its bridge dispatcher, spend book, seating and funding. The PRD's
target residual is `board`, `turns`, `phases`, `commitment` and the rules - and
the remaining game packages already look close to that shape:

| package | lines | verdict |
|---|---|---|
| `pkg/match` | 2,796 | rules: schedule, duty clocks, money matrix |
| `pkg/commitment` | 1,341 | rules (T-04 ruled it out of the SDK) |
| `pkg/bslog` | 1,179 | the game's own log vocabulary |
| `pkg/bsschema` | 629 | the game's own messages |
| `pkg/board` | 474 | rules |

None of those should move. What should is what is left in `cmd/`, and that waits
on funding.
