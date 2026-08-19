# T-15 — Money-shaped verbs: retiring `pkg/ruling`

Supersedes [t01-forfeiture-handover](t01-forfeiture-handover.md) · 2026-08-20

---

## The question

T-01 asked what a ruling is, such that the SDK can carry out a forfeiture
without knowing the game's rules. It answered: a match, a seat, a kind, and one
proof — three kinds, because there are three ways a bond leaves an escrow.

It applied half of its own decision rule. The rule, from the architecture doc,
has two arms:

> if one ruling type covers both without a game-specific branch inside the SDK,
> proceed. **If either game needs the SDK to interpret its evidence, the
> boundary is in the wrong place — move it toward the game and retry.**

T-01 tested the first arm and recorded PROCEED. The second arm was never
tested, and it fails: `Equivocation` and `Silence` are words about games, and
`ruling.Equivocated` is a proof shape the SDK took apart. A game that cheats in
a way neither word covers could not say so without the SDK learning a new kind
first.

## The answer

The SDK's vocabulary names what happens to money and never what a game
concluded. Four verbs, one predicate:

| verb | what authorises it |
|---|---|
| `Settle(match, Outcome)` | the signatures the stake escrow's own script asks for |
| `Seize(match, seat, evidence.Exposed)` | a key that opens a branch of the bond this runtime derived |
| `Accuse(match, seat, Lapsed)` | a height the chain has passed, plus the ladder's right of reply |
| `Release(match)` | the other seat's co-signature |

and `CoSigning.WillCoSign(match, seat)`, an optional hook, for the answer that
is a refusal rather than an instruction.

## Why the proof was not worth keeping

This is the load-bearing finding, and it corrects T-01 rather than extending
it.

**`forfeit.Recover` verifies neither signature.** There is no `schnorr.Verify`
anywhere in `pkg/forfeit`. `Recover` solves `d = (sA - sB)/(eB - eA)` and then
compares `d·G` against the public key **the caller passed in alongside**. The
challenge is `BLAKE256(r ‖ hash)` and does not bind that key.

So anyone holding a seat's log key `l` mints a passing proof with no curve
arithmetic: pick any `r`, any two distinct digests, any `sA`, and set
`sB = sA - (e_B - e_A)·l`. Every validation passes and the two "signatures" are
not signatures.

The proof was therefore a reversible encoding of the private key, not evidence
of anything. Three things follow:

1. **Handing over the key instead loses no security.** Possession of the
   accused's log key was necessary and sufficient under both shapes.
2. **It loses no audit trail either.** The SDK never persisted the proof — it
   recovered and dropped the four fields — and `Equivocated.At` was neither read
   nor validated by anything.
3. **T-01 finding 3's first bullet was false when written**, and it was the
   sentence the design leaned on.

What actually authorises a seizure, and always did:

- `escrow.ForfeitIndex` aggregates the offered key with this seat's punishment
  key and looks for the result among the bond's branches. A key that is not the
  accused's is in none of them. Offline, before anything is built.
- `escrow`'s `finishBondSpend` runs the assembled spend through the real
  `txscript` engine before the bytes leave the process.

Both hold only because the bond was **derived here from the roster** and never
accepted off the wire, the punisher key is this seat's own, and the branch names
this seat's own session key. Those three preconditions are written at `Seize`'s
call site, because `escrow.ForfeitIndex` explicitly disclaims them and `Seize`
is where the next reader will look.

## Why `evidence.Exposed` and not a bare key

`Seize` takes the evidence store's own type. The field is unexported, so the
only way to hold one is to have been handed it by the store — which means the
two halves that solved for it are on disk before a bond is spent against it.

**It is not a capability barrier**, and the type says so in its own doc: anyone
holding the key can record a pair that recovers it. It buys the audit trail the
old proof only appeared to give, and nothing else.

## What was not done, and why

**No `LogSeats()[seat]` equality check in `Seize`.** A draft had one. It is
redundant — the aggregate matching a branch while `exposed != logs[seat]` *is*
the rogue-key break, and the coefficient derivation is MuSig-shaped with only
one attacker-chosen point, so there is no k-sum to attack. T-01's own
correction says duplicating a security check one frame up is worse than
omitting it, because it implies the primitive does not do it.

That correction has a second clause and it is the one that mattered here: *the
test stays, as a contract pin at the boundary a game sees.* There was no such
pin. There are now two, and neither existed before this change:

- `TestSeizingWithTheSeatsOwnKeyTakesTheBond` — a seizure that works
- `TestSeizingWithAKeyThatIsNotTheSeatsSeizesNothing` — same setup, stranger's
  key, refused

The pair is the point: they differ only in the key, so the refusal is provably
about the key and not the fixture.

## Corrections to the shape as first designed

- **`Accuse` refuses `By == 0` explicitly.** `Silent.validate()` enforced this
  and a plain `uint32` field does not: the zero value is at or below every
  height, so the tip gate would wave it through and a caller that forgot the
  argument would spend the accused's bond with no stated lapse at all. There is
  no safe height to default to.
- **`Lapsed` keeps `Duty` and `Seq`, and `Accuse` logs them.** They were
  write-only under the old shape while `pkg/ruling`'s doc claimed they were
  carried into the log. Now they are.
- **`Seize` refuses `seat == our own`**, which `runLadder` already did. Without
  it a self-seizure failed four frames down as "this bond has no punishment
  branch for that key", which reads like a derivation bug.
- **`Release` takes no seat.** `Ruling.Against` was accepted and silently
  ignored on the clean path — a field that lied. Whose bond is no longer
  expressible, because a release pays its owner and nothing else.
- **`WillCoSign` is an optional hook, not a `Rules` method**, matching `Seated`
  and `Settled`. A game with nothing to say implements nothing and behaves
  exactly as before.

## What this does not fix

Worth writing down, because the change is easy to oversell. It removes
*vocabulary* coupling. What a genuinely new game hits is not vocabulary:

1. the two-seat guards in `runtime/ladder.go`, `runtime/punishkeys.go` and
   `punish/ladder.go`
2. `punish.CheckPinned`'s single-output invariant, mutually exclusive with a
   multi-taker take at more than two seats
3. `ladder.run` as a per-peer counter, safe only because exactly one peer can
   run a chain heads-up
4. `Terms.BuyInAtoms` as one uniform number for the whole table
5. cooperative-only `Settle` — no adjudicator, no non-cooperative payout
6. **cheats that expose no key at all.** A bad DLEQ proof or a permutation that
   does not match its commitment is provable and non-repudiable, and `Seize`
   cannot touch it: no exposure, no branch. Only the ladder is left, and its
   ceiling is attrition. dcrpoker already is this game.

Point 6 is why `Seize` is documented as *spend a branch whose secret the accused
exposed* and never as *punish a cheat*. The old name at least admitted it only
handled equivocation.

## Guarantees pinned by tests

- `Lapsed` names no script, address, amount or transaction.
- A seizure with the accused's own key takes the bond; the same seizure with any
  other key takes nothing.
- A seat does not seize its own bond, and says so.
- An accusation that does not say what was owed, or when, is refused.
- An accusation before the stated height is refused, and the refusal *changes
  reason* once the chain passes it.
- A game may withhold one seat's release without stopping the others, and a
  withheld seat holds the whole payout.

## Verification

`go build`, `go vet`, `go test` green in all three repos, battleships under both
build tags. Four mutations, each reddening exactly one test with no build-error
lines masking the result: the release gate, the settlement gate, the self-seize
guard, and the stranger-key pair.

Known, pre-existing, **not** introduced here: `TestBothSeatsGetTheirTableBondBack`
fails roughly one run in three under full-package load. Reproduced at `35edeb3`,
before any of this work. Both releases are broadcast; what varies is which peer
finishes them, and the test asserts each peer sent its own. That looks like the
test being stricter than the design, not a money fault, but it has not been
run down.
