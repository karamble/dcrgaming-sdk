# T-01 — The forfeiture handover: decision

**Spike outcome: PROCEED.** One ruling type covers both games with no
game-specific branch inside the SDK.

Branch `runtime-t01-forfeiture-handover` · package `pkg/ruling` · 2026-08-19

---

## The question

What exactly is a ruling, such that the SDK can execute a forfeiture without
knowing the game's rules?

**Decision rule as written:** one ruling type covering battleships' three fates
and poker's take/release with no game-specific branch → proceed. Either game
needing the SDK to interpret its evidence → the boundary is wrong.

## The answer

A ruling names **a match, a seat, a kind, and one proof**. Three kinds, because
there are three ways a bond leaves an escrow — not because three read well:

| Kind | Path | Proof |
|---|---|---|
| `Clean` | release the bond to its owner | none |
| `Equivocation` | sweep the forfeitable bond to the seat that was lied to | two signatures at one position |
| `Silence` | run the claim ladder against a seat that stopped answering | the duty and the height it lapsed at |

Mapped against both games, which is the decision rule:

| Game | Situation | Kind |
|---|---|---|
| battleships | divergent cell attestations sweep the bond | `Equivocation` |
| battleships | a seat stops answering its duty clock | `Silence` |
| battleships | match ends cleanly, table bond released | `Clean` |
| poker | a seat signs two actions at one position | `Equivocation` |
| poker | an unanswered claim taken after its window | `Silence` |
| poker | table breaks up, bonds released | `Clean` |

Pinned as a test (`TestBothGamesFitTheThreeKinds`).

## Four findings worth carrying forward

**1. The mechanism was already unified one layer down.** Both games build the
same transaction shapes from the same SDK drafts — `escrow.AccuseDraft`,
`escrow.AnswerDraft`, `escrow.AliveDraft`, `escrow.ParseClaimedBond`. Poker's
`proposeTake(against, outpoint, claimed, value)` and battleships' `punish.Take`
are the same operation. The duplication was never in the money; it was in the
deciding and the driving.

**2. Attrition is not a ruling.** It is what running `Silence` produces when the
accused keeps answering — an outcome, not an input. Same for battleships'
`Fate` values generally: they name which escrow branch the money came home
through, which is a *result* the runtime reports, never something a game asks
for. A ruling that could name its own fate would be a ruling that could direct
money.

**3. The two kinds are safe for different reasons, and this is the load-bearing
part of the design.**

- **Equivocation is verified, cryptographically, in the SDK.** Two signatures
  sharing a nonce expose the signer's key; `forfeit.Recover` either produces it
  or reports that no key is exposed. The game's assertion is *not* what moves the
  bond — the recovered key is. A game cannot cause a sweep by lying.
- **Silence is not verified, and does not need to be.** Whether a duty was owed
  is game logic the SDK has no vocabulary for. The safety comes from the
  mechanism instead: the ladder gives the accused an on-chain right of reply, so
  an accusation against a seat that is in fact alive gets answered and buys the
  accuser nothing but attrition.

**4. `schema.Duty` cannot be reused, and this is a wart the extraction left.**
Its `DutyKind` values are `cardkey`, `shuffle`, `share`, `action`, `checkpoint`,
`reveal`, and `schema.Duty` carries a `Hand`. That is poker's vocabulary,
inherited verbatim in the v0.1.0 extraction. A battleships forfeiture has no
hand and shuffles nothing. A silence ruling therefore names its duty as an
**opaque string the game chooses**, which the runtime carries but never
interprets.

Related: `forfeit.Domain` is half-poker too (`DomainCardKey`, `DomainShuffle`
beside the generic `DomainEntry`, `DomainHead`). Not blocking — `Domain` is a
string and a game may define its own — but it belongs on the list of
poker-shaped leftovers to revisit.

## Guarantees pinned by tests

- A ruling names no script, address, amount or transaction.
  `TestARulingNamesNoMoney` pins the exact field set of all three types and
  fails if anyone adds one that does. The worst a wrong ruling can do is punish
  the wrong seat *of its own table*.
- Two honest signatures at different positions expose nothing.
- A ruling cannot borrow a third party's equivocation.
- Exactly one proof, and it is the one the kind names.
- The same statement signed twice is not equivocation.

## What this spike deliberately did not do

No runtime wiring. `Silent` states its contract — the runtime must refuse to
accuse before the chain tip passes `By` — but that check belongs to **T-12**,
where the ladder lives. Verification of the equivocation proof is here because
it is pure arithmetic over the proof itself.

## Corrections made during the spike

A first draft of `Recover` re-checked that the recovered key matched the named
one. Mutation testing showed the check was dead: `forfeit.Recover` already ends
with exactly that comparison (`sign.go:413`). Duplicating a security check one
frame up is worse than omitting it — it implies the primitive does not do it,
and a reader has to verify both. Removed; the test that covers the behaviour
stays, as a contract pin at the boundary a game sees.

## Verification

`go build ./...`, `go vet ./...`, `go test ./...` all exit 0; 10/10 packages
pass. Three mutations were introduced and each reddened a distinct test, with no
build-error lines masking the result:

| Mutation | Caught by |
|---|---|
| swallow the error from `forfeit.Recover` | `TestTwoHonestSignaturesExposeNothing`, `TestARulingCannotBorrowAnotherSeatsEquivocation` |
| drop the same-statement-twice guard | `TestTheSameStatementTwiceIsNotEquivocation` |
| let a mismatched proof through `Validate` | `TestOnlyTheProofTheKindNamesMaySit` |

## Unblocks

**T-05** (the `Rules` interface — rulings are one of the four things a game
supplies) and **T-12** (forfeiture execution — this is its entry point).
