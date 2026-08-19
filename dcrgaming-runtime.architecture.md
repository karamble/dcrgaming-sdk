# Architecture — the dcrgaming runtime

**Status**: Decided, pending spikes · **Date**: 2026-08-19 · **Intent**:
[dcrgaming-runtime.prd.md](./dcrgaming-runtime.prd.md)

High-level decisions taken before implementation. Not a task plan.

---

## Problem & goals

A developer arrives with a finished game — board, rules, turns, UI, two players in a
session — and wants to add dcrgaming money support by configuring the SDK rather than
implementing escrow, seating, settlement, forfeiture and reclaim themselves.

Every decision below is judged against the PRD's acid test: **rebuild the existing
battleships daemon on the runtime, and the residual game-owned code should be its rules
and nothing else.**

## Approaches considered

**Library — the game keeps its loop and calls runtime functions.** Closest to both games
today; each already runs its own `reg.loop(ctx)` and `serveBridgeRequests`. Cheapest
migration by a wide margin. Rejected because the runtime cannot guarantee correctness it
does not control: if the game calls `put` on the spend book, the book can still be written
wrong, and "zero lines of money code" fails by construction.

**Engine — the runtime runs its own goroutines and exchanges typed events.** Strong
isolation, and it maps onto the genuinely async reality of bridge events, chain
confirmations and counterparty messages. Rejected because a game and its runtime are one
process, so the isolation buys little, while the concurrency contract becomes public API
forever and the game side becomes harder to test.

**Framework — the runtime owns the lifecycle loop and calls into the game.** Chosen. It is
the only shape that reaches the acid test, because owning the loop is what lets the runtime
own correctness.

## Recommended approach

New packages inside the existing `dcrgaming-sdk` module own the table lifecycle. The
runtime drives **invite → seat → fund → settle → forfeit → reclaim**, holds the durable
money record, serves the bridge's five control requests, and manages the bridge
connection.

A game supplies four things and nothing else:

1. **Identity and terms** — its name and version, seat count, buy-in, bond amounts and
   locks, admission window.
2. **Its own traffic** — the message kinds it sends and the handlers that receive them.
   The runtime frames, routes and reassembles; it never inspects the payload.
3. **An outcome** — who won, or how the pot splits.
4. **Rulings** — a typed decision that a seat has forfeited, with the evidence the
   punishment path must pin.

The game keeps its rules, its duty clocks, and its interface. Nothing about boards, cards,
turns or phases enters the SDK.

Where it plugs into what exists: the runtime sits directly on the current primitives —
`escrow` for scripts, `membership` for roster and terms, `forfeit` for punishment keys,
`gamelog` for the signed chain, `gaming/{schema,wire,transport}` for messaging and the
bridge client, `identity` for the seed. It adds the state machines those primitives were
always missing, and it absorbs battleships' `pkg/punish` and `pkg/evidence`, which are
already game-agnostic in everything but two couplings.

## Key decisions

### Stack & libraries

**No new dependencies.** The runtime builds only on what the module already pins:
`decred/dcrd` (`txscript/v4 v4.1.1`, `wire v1.7.0`), `secp256k1/v4`, `google.golang.org/grpc
v1.83.0`, `decred/slog`, and the standard library.

Considered and rejected: a workflow or state-machine library, and a embedded key/value
store for the spend book. Both were rejected for the same reason — the pinned versions
guard bytes that hold live mainnet bonds, MVS takes the maximum across the graph, and every
dependency added to the SDK is a new way for a transitive bump to move those bytes. The
existing CI pin-check exists precisely because that surface is already uncomfortably wide.

### Module layout

**Same module, new packages under `pkg/`.** Considered a separate `dcrgaming-runtime`
module for a cleaner primitives-versus-runtime split, and rejected it: it doubles the
pin discipline across two `go.mod` files under MVS, and it opens a live path to a binary
linking two `gamingpb` copies, which panics at init before a single test runs. That is a
frozen wart, not a preference. Unused packages cost a consumer nothing, so the split buys
nothing to offset the risk.

### Data model

At the shape level. Entities the runtime owns:

- **Identity** — exists (`pkg/identity`): the seed, and per-game keys derived from it.
- **Table and Seat** — the roster, terms, match id, seat credentials, payout address.
  `membership` holds the primitives; the lifecycle state around them is new.
- **Terms** — `membership.Terms` exists and is already generic (`Game`, `GameVer`, `SID`,
  `BuyInAtoms`, `Seats`, `CSVBlocks`, `Until`) but carries **no bond terms**; battleships
  keeps `BondLockBlocks` in its own `match` package. Terms must grow a bond vocabulary, or
  gain a companion type, so a game can declare stake and bond locks in one place.
- **Spend record** — the unification of the two incompatible `pendingSpend` types. One
  record, one state machine, with unreachable-versus-refused as states rather than as a
  remembered convention.
- **Bond** — stake, table and forfeitable bonds, their scripts, outpoints and maturities.
- **Ruling** — new, exists in neither game: the game's typed forfeiture decision plus the
  evidence to pin.
- **Outcome** — the settled result the payout is built from.

**Storage**: the runtime owns the writes, following the precedent `pkg/identity` already
sets — JSON under the game's appdata, `0600`, temp-then-rename. Every state change is also
emitted so a game can mirror into its own database for its own queries. A `Store` interface
exists as an escape hatch for a game that needs one transactional store across its own
state and the money state, documented as transferring correctness to the implementer.

### Boundaries & contracts

- **The bridge remains the sole policy enforcer.** The runtime asks; it never decides that
  money may move. Per-game caps and human approval with the wallet passphrase stay
  bridge-side, unchanged.
- **No secrets in the SDK's hands beyond the seed.** The wallet passphrase never reaches a
  game or the runtime; `pkg/identity` already owns the seed and its lost-seed guard.
- **The SDK never rules.** Cheat detection, discrepancy adjudication, evidence evaluation,
  challenge and complaint protocols, and duty clocks all stay with the game. The runtime's
  forfeiture entry point accepts a decision already made.
- **Game traffic stays opaque**, routed by game id, framed and reassembled by the existing
  `wire.Router` / `Publisher` / `Receive`.
- **The frozen warts are hard constraints**: proto package `dcrpulse.gaming.v1`, the bare
  descriptor path, one `gamingpb` per binary, the 17 domain-separation tags, the
  `txscript`/`wire` pins, `escrow.MaxMembers = 6`, and no protoc target in this module.

### Other calls

- **Duty clocks stay with the game** (maintainer's call, stricter than the recommendation
  offered). Consequence to design for: because the runtime owns the loop but the game
  watches its own deadlines, the runtime must expose **chain tip and the signed log as a
  read surface** to the game. That read surface is part of the public API and should be
  designed deliberately rather than leaking whatever the runtime happens to hold.
- **`pkg/punish` and `pkg/evidence` move to the SDK**, less two couplings that must be cut
  first: `Attrition.Fate()` / `Outcome()` map into battleships' spec-8.5 money matrix, and
  the ladder carries battleships' fee and depth constants. `ShouldAccuse` does **not**
  move — it is duty detection, and duty belongs to the game.
- **Poker proves genericity on a branch that is never deployed.** Its real call sites must
  compile against the runtime and its byte-identical goldens must pass. Deployment to live
  daemons is a separate, later decision, taken deliberately.
- **Mainnet is the test environment.** No testnet stage.
- **Every package lands on its own branch**, in the SDK and in each consuming game. Nothing
  goes to a mainline directly.

## Missing pieces

What the chosen approach needs that does not exist anywhere yet:

- **The `Rules` interface.** Neither game has anything resembling it; both are written as
  drivers, not as implementations. This is the central new artifact.
- **A forfeiture vocabulary** the SDK can act on without knowing the game.
- **A unified spend record and its state machine**, replacing two incompatible types.
- **Bond terms in the shared vocabulary** — `membership.Terms` has none.
- **A supported bridge test double.** Both games built their own; a third-party developer
  cannot test money handling without one.
- **The chain-tip and log read surface** the game needs for its own duty clocks.
- **Written contract** sufficient to use all of the above without reading dcrpoker.

## Spikes & experiments

Three one-way calls to de-risk before committing.

**1. The forfeiture handover.** *(Ran; PROCEED, then reversed on the rule's own
second arm — see [t15](docs/decisions/t15-money-shaped-verbs.md). The ruling type
is gone and the SDK's forfeiture vocabulary now names money rather than games.)*
- *Question*: what exactly is a ruling, such that the SDK can execute it without knowing
  the game's rules?
- *Spike*: define the ruling and evidence types, then check they express battleships'
  three fates (sweep on equivocation, take on silence, attrition on an answered-out
  ladder) **and** poker's take/release paths, using both games' existing call sites as the
  test.
- *Decision rule*: if one ruling type covers both without a game-specific branch inside the
  SDK, proceed. If either game needs the SDK to interpret its evidence, the boundary is in
  the wrong place — move it toward the game and retry.

**2. The `Rules` interface against poker, not battleships.**
- *Question*: is the interface generic, or battleships-shaped?
- *Spike*: sketch it, then port poker's seating and settlement call sites against it on a
  branch — poker first, deliberately, because designing against battleships and validating
  against battleships proves nothing.
- *Decision rule*: if poker needs a method battleships does not, the interface is
  poker-shaped and the extra method is probably adjudication leaking in. If poker fits with
  no additions, commit the shape.

**3. Commit-reveal: shared primitive or game logic?**
- *Question*: battleships commits to a fleet layout (`pkg/commitment`, 1,341 lines), poker
  commits to cards. Is the scheme common even though the content is not?
- *Spike*: compare the two schemes directly for a common core.
- *Decision rule*: if a common core exists that neither game has to bend to, it joins the
  SDK. If either has to bend, both keep their own — a shared abstraction that fits neither
  is worse than two that fit.

## Open questions

Deferred deliberately, with what would settle each.

- [ ] **Does `membership.Terms` grow bond fields, or gain a companion type?** Settled by
      the shape battleships and poker's bond terms actually share.
- [ ] **How much of poker's forfeiture path is replaced?** Poker's adjudication stays, but
      it sits on its own ad-hoc mechanism rather than on battleships' ladder. Settled by
      spike 1.
- [ ] **What is the compatibility promise?** Three tags in three days, one consumer pinned
      to the oldest. Settled by a written policy before the runtime is tagged.
- [ ] **When, if ever, do poker's live daemons adopt the runtime?** Deliberately separate
      from the genericity proof. Settled by a maintainer decision after spike 2.
- [ ] **If a non-Go game appears, does the sidecar option return?** Settled by a real
      request, not in advance.
