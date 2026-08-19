# T-04 — Commit-reveal: decision

**Spike outcome: NO. It stays with the game.** There is no common core, because
only one of the two games has a commitment at all.

Branch `runtime-t04-commit-reveal` · no code · 2026-08-19

---

## The question

Battleships commits to a fleet layout (`pkg/commitment`, 1,341 lines) and poker
commits to cards. Is the scheme common even though the content is not?

**Decision rule as written:** a common core neither game has to bend to → it
joins the SDK. Either has to bend → both keep their own.

## The answer

The premise was wrong. The two games do not use two variants of one scheme; they
use two unrelated constructions, and the question of a shared core does not
arise.

**Battleships: a Merkle commitment.** `Commit` produces a root over a fleet
layout, `Open` produces `Openings` with a `Proof` per cell, and `VerifyOpen`
checks one against the root. The property it buys is that opening one cell
reveals nothing about the others - pinned by
`TestSiblingIsHiddenByRoot`. Around it sit `CellDigest`, `ShipDigest`,
`AttestCell`, `AttestShip` and `RevealVerify`.

**Poker: mental poker.** No commitment, no opening, no Merkle tree. The deck is
collaboratively encrypted and shuffled, and a card becomes visible when enough
seats publish decryption shares: `ShuffleFrom`, `ShareFrom`, `CardKeyFrom`,
`SecretsFrom`, `DeckBytes`. Hidden information is protected by threshold
decryption, not by a hash of something withheld.

There is nothing to factor out. A "shared commit-reveal package" would be
battleships' package with a second consumer that never calls it.

## Why the two designs differ, and why that is correct

The constructions differ because the games hide different things from different
people. Battleships hides a static layout, fixed before play, from exactly one
opponent - a commitment is the cheapest thing that pins it. Poker hides a deck
whose order must be jointly randomised so that no seat, including the dealer,
knows it - a commitment cannot express that, because there is no single party
who legitimately knows the answer to commit to.

A game with a static secret one party legitimately holds wants the first. A game
with a secret nobody may hold wants the second. That is a property of the game's
rules, which is the line the runtime already draws.

## What *is* shared, and is already in the SDK

The interesting overlap is one layer down and already extracted. Battleships'
attestations are signed at forfeit positions, so two contradictory attestations
of one cell publish the signer's key - `TestCellAttestDivergenceRecoversKey` and
`TestShipAttestDivergenceRecoversKey` pin it. That machinery is `pkg/forfeit`,
which both games use.

So the shared part is *binding a claim so contradicting it costs you*, not
*hiding the claim in the first place*. The SDK has the first. The second is game
logic.

## Consequence for the runtime

None. No package is added, no scope changes, and the acid test's expected
residual is unaffected: `pkg/commitment` was already counted as battleships'
own code that stays with battleships.

One line in the PRD needs no correction but is worth reading again in this
light - the open question asked whether the scheme "may be common even though
the content is not". It is not, and the reason is sharper than a scope call: two
games can both need hidden information and share no cryptography whatsoever.

## Unblocks

Nothing was blocked on this. It was carried as deferrable and is now closed.
