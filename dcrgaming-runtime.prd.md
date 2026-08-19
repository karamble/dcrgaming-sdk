# PRD: The dcrgaming runtime

**Status**: Draft · **Created**: 2026-08-19 · **Owner**: karamble

A product-level PRD for the missing runtime layer of `dcrgaming-sdk`. This document is
intent — the problem, the bet, and how we would know we were wrong. It deliberately makes
no engineering decisions; those belong to the architecture spec that follows it.

---

## 1. Problem statement

A developer who wants their game to plug into dcrgaming must implement the money and
lifecycle machinery themselves — bridge connection, payment requests and their durable
state, seating and invites, settlement, forfeiture and fund reclaim — before a single
rule of their game runs. The SDK ships the primitives these are built from, but not the
machinery, so each game builds it again.

The cost is not effort. It is that **money-safety knowledge does not travel with the
SDK**. Each game re-derives, from scratch and unaided, the conditions under which a
player is paid twice or a bond is stranded. One of those conditions has already cost
real coin on mainnet. A third-party developer gets no protection from repeating it,
because nothing in the SDK enforces or even states the rule.

The SDK's own README asserts the boundary it fails to hold:

> *"What lives here is exactly the part that does not care which game is being played."*

Every duplicated function below is game-agnostic by that definition, and none of them
are in it.

## 2. Evidence

All figures measured 2026-08-19 against `dcrpoker@sdk` (0566e95) and
`dcrbattleships@master` (5e0e72b).

**The SDK's only money helper is unsafe, and both games route around it.**
`transport.AwaitSpend` (`pkg/gaming/transport/grpc_calls.go:121`) returns on any error
from `SpendStatus`, including an unreachable host. Poker wraps it in a retry loop
(`spend.go:192`) whose comment records why:

> *"Giving up on the first refused connection is what turned a restart into a payment
> made twice."*

and elsewhere (`spend.go:228`):

> *"Recording it as refused here once cost a real 0.01 DCR: the stake was paid, this
> stopped watching, the interface still said it was owed, and it was paid a second
> time."*

Battleships does not call `AwaitSpend` at all; it wrote its own loop with an explicit
`transport.Unreachable(err)` branch (`spend.go:239`). Two games, two different
corrections, same unsafe helper, neither fixed.

**61 identically-named functions** exist across the two game daemons (poker: 545
non-test functions; battleships: 228). They cluster exactly on the machinery, not the
rules: `loadBridge` `saveBridge` `runWizard` `proveItWorks` `stampGameIdentity` ·
`serveBridgeRequests` `doRequest` `handleFund` `handleTables` `gameState` · `doReclaim`
`sweep` `isSweeping` `payoutAddress` `setPayout` · `acceptInvite` `join` `setNames` ·
`proposeSettlement` `adoptSettlement` `settleDraft` `lockFacts` `accusationsReady`
`answerClaim`.

**Both games hand-wrote the same five-case dispatch** over the bridge's control surface
— `AcceptInvite`, `Reclaim`, `SetPayout`, `SetNames`, `RefreshState` — at
`poker/bridgereq.go:71` and `battleships/bridgereq.go:30`. The SDK ships the generated
protobufs and no server for them, so the plug-in point itself is reimplemented per game.

**Line-level identity** on same-named files, comments and whitespace stripped:

| file | poker | battleships | identical | % of bshps |
|---|---|---|---|---|
| `connect.go` | 158 | 145 | 133 | 91% |
| `bridgereq.go` | 167 | 95 | 38 | 40% |
| `reclaim.go` | 68 | 126 | 39 | 30% |
| `spend.go` | 175 | 174 | 24 | 13% |
| `fund.go` | 197 | 251 | 22 | 8% |
| `tables.go` | 767 | 579 | 56 | 9% |

The low percentages are the worse finding: same function names, bodies retyped. Those
are the same algorithms written twice, not copied twice.

**The record of money in flight is defined twice, incompatibly.** `pendingSpend` exists
in both games with divergent fields — `SID`+`Seat` vs `Match`, `AtAtoms` vs `Atoms`,
`Outpoint` vs `Vout`, and an `Unreachable` field present in one and absent in the other.

**Battleships wrote roughly 2,078 non-test lines of money machinery** before any
battleships rule ran: `money.go` 822, `fund.go` 572, `spend.go` 358, `reclaim.go` 251,
`recover.go` 75 — plus `connect.go` 275 and `bridgereq.go` 173 of bridge plumbing.

**Both games built their own bridge test doubles.** Poker: `harness_test.go`,
`hub_test.go`. Battleships: `relayBridge`/`startRelay` in `fakebridge_test.go`, plus
`fakeBridge` and `fakeHost`. A third developer builds a third.

**What the SDK does cover well, and is not in scope here:** Bison Relay messaging.
`wire.Router`, `NewPublisher` and `Receive` already handle framing, out-of-order chunk
reassembly, per-sender chunk isolation, unauthorised-sender rejection and other-game
filtering, with tests. The gap is the layer above the transport client, not the
transport.

**Scope note.** The project is in development and its author is currently its only user;
no third-party game exists yet. All evidence above is therefore in-house — and it is
sufficient, because the two games are genuinely independent implementations that disagree
in ways that have cost coin. Enabling a third party is the design goal, not a present
constraint.

## 3. Thesis

The first extraction was gated on **byte-identity** — prove the moved code emits the same
bytes it did before. That gate is what made it safe under live coin, and it is also why
the SDK contains only nouns. Scripts, key derivation, encoders and hashes can pass a
byte-identity proof. A spend book with a mutex and a file, a polling await loop, a
seating lifecycle cannot. The orchestration was excluded by the very criterion that made
the extraction trustworthy, and it was then written twice.

Why now: battleships is the first independent test of the claim, and it failed it
quantitatively. The genericity criterion the bridge spec did measure — SC-005, *"a second
unrelated game type connects and transacts with no bridge code changes"* — was met. But
SC-005 only ever measured the **bridge's** cost of a second game. Nobody measured the
**game's**, and it turned out to be thousands of lines with a re-derived money-safety
invariant inside.

Why this beats the alternative: the alternative for a developer today is to read
dcrpoker and copy it. That transmits the code but not the reasoning — the reasoning
lives in comments in one game's private source, and battleships only avoided the
double-payment bug because the same person wrote both. That does not survive contact
with a third party.

## 4. Hypothesis

> We believe that moving the money and lifecycle **runtime** into the SDK — not just its
> primitives — will cause a game developer with no Decred background to reach a funded,
> settled, reclaimable table without authoring spend persistence, retry or reclaim logic,
> resulting in a game whose money handling is the same code as dcrpoker's mainnet-proven
> path rather than a re-derivation of it.
>
> We'll know we're **RIGHT** if a game built against the runtime by someone who has not
> read dcrpoker's source completes invite → seat → fund → settle → forfeit → reclaim,
> having written zero lines of membership, spend-book, settlement, forfeiture or reclaim
> code.
>
> We'll know we're **WRONG** if that developer still has to write or override money-path
> code to make it work; or if the runtime cannot express dcrpoker's existing behaviour
> without a game-specific branch inside the SDK; or if migrating dcrpoker onto it changes
> any byte of a live escrow script, punishment key or signed log.

The third wrong-condition is a guardrail, not a signal: it fails the bet outright rather
than merely weakening it.

## 5. Target user & JTBD

**Primary user**: a Go developer building a multiplayer game with real stakes, who wants
Decred's trustless-table properties and has no intention of becoming an expert in
escrow scripts, forfeiture keys or Bison Relay framing.

**JTBD**: *I already have a finished game — board, rules, turns, UI, two players in a
session. When I want those players to stake real value on it, I want to add dcrgaming
money support by configuring the SDK, not by implementing escrow, seating, settlement,
forfeiture and reclaim myself — so I can ship without becoming responsible for a class of
bug that costs my players coin.*

The developer arrives with a working game and one sentence: **"give me the SDK."** They
should never need to learn what an HMAC domain tag, a forfeitable-bond ladder or a gRPC
`:path` is in order to answer it.

**Secondary user**: the maintainer starting game #3, who should not begin by copying
files out of battleships.

**Non-users**: developers wanting a custodial or centralised backend; developers wanting
the SDK to know their game's rules; anyone needing a non-Go client library — the
published protocol is for them, not the module.

**Constraints carried in** (decided, not open):

- Distributed as a **Go module**, with the **protocol published** to the standard where
  another language could reimplement it. No second-language client library.
- A game must be able to **let the SDK persist** its money and lifecycle state, **or
  query the SDK and persist to its own database**. Both must be possible.
- **Two sources of record, at different layers.** dcrpoker for the money and membership
  lifecycle — its path is mainnet-proven. Battleships' `pkg/punish` and `pkg/evidence` for
  the forfeiture mechanism — purpose-built to generalise, not yet run on mainnet. Poker's
  dispute adjudication is not an input at all.
- **Mainnet is the test environment.** No testnet stage, consistent with the project's
  standing rule that assertions are made against mainnet.
- **Work lands on separate branches**, per package, never directly on the mainline of
  either the SDK or a consuming game.
- The frozen warts hold: proto package `dcrpulse.gaming.v1`, the bare descriptor path,
  one gamingpb per binary, the 17 domain-separation tags, the `txscript`/`wire` pins.

## 6. MVP

The thinnest line that proves or kills the hypothesis end to end:

**One game runs the whole lifecycle — invite → seat → fund → settle → forfeit → reclaim —
using only the runtime, with no membership or money code of its own; and dcrpoker's
behaviour is reproducible through the same runtime without a game-specific branch inside
the SDK.**

The runtime owns all six stages. A game supplies its rules and nothing else.

1. **Membership and seating.** Invite, accept, join, roster formation, terms agreement,
   match-id binding, names, payout address. `pkg/membership` already holds the primitives
   — formation, terms, roster, forfeitable-bond construction — but the state machine that
   drives them (`acceptInvite`, `join`, `seatTable`, `setNames`, `setPayout`) is written
   once per game.
2. **The money lifecycle** as durable machinery rather than per-call wrappers: one record
   of money in flight, one persistence path, with unreachable-versus-refused enforced by
   the runtime instead of remembered by the author. Both persistence modes — SDK-owned
   and game-owned — exercised.
3. **Settlement.** Propose, draft, co-sign, adopt, broadcast, and the backstop for a
   counterparty that stops answering. Shared by name across both games today
   (`proposeSettlement`, `settleDraft`, `adoptSettlement`) and implemented twice.
4. **Forfeiture execution — not adjudication.** The game rules *that* a seat forfeits; the
   SDK carries it out. Punishment-key announcement and its proof of possession,
   forfeitable-bond construction, the bond ladder and its co-signing, the release path and
   its backstop, expiry by silence, attrition bounds, and sweeping a forfeited bond.
   `pkg/forfeit` holds the crypto; the mechanism that drives it lives in battleships'
   `pkg/punish` (1,501 lines: `BuildLadder`, `CoSignAccuse`, `CoSignRelease`,
   `BackstopRelease`, `SweepForfeited`, `TakeExpired`, `AttritionBound`) and
   `pkg/evidence` (555 lines: the equivocation store), neither of which knows anything
   about ships. Both belong in the SDK.
5. **Reclaim.** The sweeper and the reclaim kinds, so locked funds return to the operator
   after a timelock without game-specific code.
6. **The plug-in surface, connection and harness.** The SDK serves the bridge's five
   control requests so a game supplies behaviour rather than a dispatcher; the connection
   lifecycle (configure, identify, dial, prove, reconnect) minus any console; a bridge
   test double shipped as a supported artifact; and enough written contract that all of
   it is usable without reading dcrpoker.

**The acid test — and it is not hypothetical.** Battleships already exists: board,
commitment, match rules, turn engine, UI, all finished. Rebuild its daemon on the runtime
and whatever a developer still has to hand-write is the SDK's remaining gap. The target
is that they write their rules, name their game, declare their stake and bond terms, and
hand the runtime a typed ruling when their own rules decide a seat has forfeited. They
write no bridge dispatcher, no spend book, no bond ladder, no sweeper and no seating state
machine — and they never see an HMAC tag, an escrow script or a gRPC call. Measured
against today's daemon, the residual should be `board`, `turns`, `phases` and `commitment`
and nothing else.

**Two sources of record, at different layers — not a conflict.** The money and membership
lifecycle comes from dcrpoker, whose path is mainnet-proven. The forfeiture mechanism
comes from battleships' `pkg/punish` and `pkg/evidence`, which were purpose-built to
generalise. Poker's challenge / complaint / claim / audit machinery is **not an input**:
deciding a player cheated is the referee's job and stays with the game. Because the two
sources sit at different layers, nothing has to be reconciled between them.

**Door check.** The runtime's API shape is a **one-way door** for everything dcrpoker's
live daemons will consume: poker is pinned at v0.2.0 and holds live coin, so an API it
adopts is expensive to reshape. Spike the lifecycle API against both games' existing call
sites before committing to it. Forfeiture carries a harder one-way door — any change to
bond scripts or punishment-key derivation invalidates live bonds — and it is not done
until it has run on mainnet against a real refusing counterparty. Mainnet is the test
environment; there is no testnet stage.

## 7. Success metrics

| Metric | Baseline | Target | How measured |
|---|---|---|---|
| Money-path lines a new game must author | 2,078 (battleships) | 0 for spend persistence, retry and reclaim | Diff a new game's daemon against the runtime API surface |
| Lifecycle lines a new game must author (membership, seating, settlement, forfeiture, reclaim, bridge plumbing) | ≈3,700 (battleships, non-test, mixed with its rules) | 0 for the machinery; the game keeps only its rules | Same diff; the residual should be board, turns and phases only |
| Mainnet forfeiture runs completed against the runtime | 0 | ≥ 1, with a real refusing counterparty | Observed on mainnet, bonds accounted for |
| Identically-named functions across shipped games | 61 | ≤ 10, all genuinely game-specific | Same function-name comparison used in §2 |
| Distinct `pendingSpend`-equivalent definitions | 2 | 1 | Grep the ecosystem |
| Money helpers that abandon an in-flight payment on an unreachable host | 1 (`AwaitSpend`) | 0 | A test that fails if any money helper returns on `Unreachable` |
| Bridge test doubles written by consumers | 2 | 0 | Consumers import the shipped double |
| Bytes changed in live escrow scripts, punishment keys or signed logs by the migration | — | 0 | Byte-identity proof, as used in the first extraction |
| Time for a Go developer with no Decred background to reach a funded table | TBD — no baseline | TBD — needs validation | First external attempt, observed |

## 8. Non-goals

- Game rules of any kind: referees, decks, boards, turn engines, phase machines.
- **Deciding that a forfeiture is warranted.** Cheat detection, discrepancy adjudication,
  evidence evaluation, challenge and complaint protocols, card audits — all game logic.
  The SDK executes a forfeiture the game has already ruled on; it never rules.
- **UI of any kind**: consoles, HTTP servers, token gates, view projections. A game may
  be a terminal client, a 3D app or headless.
- A second-language client library. The protocol is published; the module stays Go.
- Any change to the bridge-side contract, the proto package path or the descriptor path.
- Renormalising the 17 domain-separation tags, or bumping the `txscript`/`wire` pins.
- Shipping a protoc target in the SDK — dcrpulse remains generator of record.
- Unattended or automatic pay-in. Human approval at the bridge is unchanged.
- Finishing `specs/001-standalone-gaming-bridge` — that is the bridge's side of the wire
  and a separate effort.

## 9. Open questions

- [ ] Where exactly does the game hand a forfeiture to the SDK? The game rules that a seat
      cheated or went silent; the SDK needs that as a typed decision plus whatever the
      punishment path must pin as evidence. That handover is the boundary the whole split
      rests on, and it is the first thing to design.
- [ ] Does the published protocol need a conformance suite, or is prose plus the proto
      sufficient for another language to reimplement correctly?
- [ ] When does dcrpoker migrate? It is pinned at v0.2.0, is not on a `replace`, and its
      daemons hold live coin. Migration is the strongest proof of genericity and the
      largest risk to real funds.
- [ ] How much of poker's existing forfeiture path is replaced when the mechanism moves
      to the SDK? Poker's adjudication stays, but it currently sits on its own ad-hoc
      mechanism rather than on battleships' ladder, so the seam between them has to be
      found before poker can migrate.
- [ ] Is commit-reveal a shared primitive or game logic? Battleships commits to a fleet
      layout (`pkg/commitment`, 1,341 lines) and poker commits to cards. The *scheme* may
      be common even though the content is not — a boundary case the acid test will
      expose.
- [ ] If a non-Go game does appear, does the sidecar option return, and does that
      retroactively change the runtime's API shape?
- [ ] What is the compatibility promise? The SDK has three tags in three days and one
      consumer pinned to the oldest.
