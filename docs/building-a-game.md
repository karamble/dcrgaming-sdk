# Building a game on dcrgaming-sdk

You have a finished game - board, rules, turns, an interface, two players in a
session - and you want those players to stake real value on it. This is what you
have to write, and, more usefully, what you do not.

You should not need to read dcrpoker to follow this. If you do, that is a bug in
this document.

---

## What you are plugging into

A **dcrpulse gaming bridge** is somebody's own appliance. It holds the wallet
and the Bison Relay identity; your game holds neither and never will. Your game
runs wherever you like, dials the bridge, and asks it to move money. A person at
the dashboard approves every payment with their wallet passphrase.

That shape is the whole security model, and two consequences follow from it:

- **Your game cannot pay anybody.** It can only ask. If your design needs
  unattended payments, this is the wrong substrate.
- **Your game cannot be reached from outside.** It dials out; nothing dials in.
  No inbound port, no port forwarding, NAT is fine.

## The four things you write

```go
type Rules interface {
	Identity() connect.Identity
	Terms(sid string) (membership.Terms, error)
	Handle(ctx context.Context, in Message) error
	State(ctx context.Context) State
}
```

**`Identity`** - what your game is called and which version of its protocol it
speaks. The bridge routes on this and checks it rather than believing it.

**`Terms`** - what a table costs: buy-in, seats, refund timelock, bond, and the
height after which no more players are admitted. Return an error to refuse a
table you will not sit at.

**`Handle`** - one message addressed to your game. The runtime has already
framed it, routed it, reassembled it across chunks and checked the sender; the
body is whatever you put there and the runtime has not looked inside. Errors are
logged and the message dropped, deliberately: a runtime that retried on your
behalf would replay moves.

**`State`** - one line and a table list, for the operator's dashboard. Called
from the bridge's request loop, so do not block on your own locks and do not
call back into the runtime.

Two optional hooks, `Seated` and `Settled`, tell you when a table has formed and
when its payout is on the chain. Implement them if you want them.

## The two things you tell the runtime

These are decisions only your rules can make, so they are calls you make rather
than methods you implement.

```go
game.Settle(ctx, match, runtime.Outcome{Shares: map[uint32]int64{0: pot}})
game.Forfeit(ctx, ruling.Ruling{ /* ... */ })
```

**An outcome** is shares, not a winner - a draw, a split pot and a three-way are
all outcomes, and `Void` unwinds a table by returning each seat its own stake.
The runtime does not check who won, because it cannot. It does check that the
shares name real seats and add up to what the table holds, because a settlement
that does not add up is one the other seats refuse, and then everybody's money
waits out a refund timelock over an arithmetic mistake.

**A ruling** says a seat has forfeited. Deciding that is your job - what counts
as cheating at your game is not something an SDK can know. Carrying it out is
the runtime's. See below.

## What you do not write

Not because it is provided as a convenience, but because writing it again is how
money gets lost:

- The bridge's control requests. There are five, and the runtime answers them.
- The record of money in flight, and its persistence.
- Bison Relay framing, chunking and reassembly.
- Escrow scripts, bond scripts, addresses.
- Seat keys, log keys, forfeit keys, punishment keys.
- The bond ladder, the sweep, the release and their backstops.
- Reclaiming your own locked money after a timelock.

You should never type an HMAC tag, an escrow script or a gRPC call.

## Forfeiture: you rule, the SDK executes

This is the boundary most worth understanding.

Your game decides **that** a seat forfeited. The SDK decides **how the money
moves**. There are three kinds because there are three ways a bond leaves an
escrow:

| Kind | What happens | What you supply |
|---|---|---|
| `Clean` | the bond is released to its owner | nothing |
| `Equivocation` | the bond is swept to the seat that was lied to | the two signatures |
| `Silence` | the claim ladder runs against a seat that stopped answering | the duty and the height it lapsed |

The two that punish are safe for different reasons, and it is worth knowing
which is which.

**Equivocation is verified.** Two signatures at one position that share a nonce
expose the signer's key - that is arithmetic, not judgement. Your assertion is
not what moves the bond; the recovered key is. You cannot cause a sweep by being
wrong.

**Silence is not verified, and does not need to be.** Whether a duty was owed is
your logic, and the SDK has no vocabulary for it. What makes it safe is the
mechanism: the ladder gives the accused an on-chain right of reply. Accuse a
seat that is in fact alive and it answers, the ladder runs out, and you have
bought nothing but attrition - bounded, and `punish.AttritionBound(fee)` tells
you by how much before anyone bonds.

**Duty clocks are yours.** The runtime exposes the chain tip and the signed log;
deciding what a seat owed and by when is game logic and stays with you.

## The failure that costs money

If you read one thing here, read this.

There are two ways to fail to get a yes from the bridge, and they are not the
same:

- **The bridge answered no.** Terminal. The money did not move and never will.
- **You could not ask.** Not an answer. The bridge may have paid while your
  question was in flight - a restart of the bridge is exactly when that happens.

Treat the second as the first and you pay twice. This is not hypothetical:
dcrpoker's source carries the receipt, and it cost 0.01 DCR on mainnet - the
stake was paid, the game stopped watching, the interface still said it was owed,
and it was paid again.

The runtime handles this for you. `spend.Unknown` is a state, not an error
return: a request in it stays open, is asked about again, and survives a restart
still open. Nothing can move a request out of open because a call failed.

If you ever find yourself writing a retry loop around a payment, stop - the
runtime already has one, and yours will not survive a restart.

## Testing without a bridge

`pkg/gaming/bridgetest` is a bridge that only exists in your process. It speaks
the real protocol over real mTLS on a real socket, so your transport is exercised
rather than stubbed.

Use its failure injection, not just its happy path:

```go
fake.SetUnreachable(true)   // every call answers codes.Unavailable
fake.SetVerdict(bridgetest.Hold, "")    // a person has not decided yet
fake.SetVerdict(bridgetest.Refuse, "over the cap")
```

Both games that reached mainnet built one of these privately first. It is
shipped so a third does not have to.

## The things you must not change

Some values in this module are frozen because live coin depends on them. They
look arbitrary. They are load-bearing.

- The proto package is `dcrpulse.gaming.v1`, baked into every gRPC path a live
  bridge routes on.
- A binary links this module's `gamingpb` **or** a vendored copy, never both.
  Double registration panics at init, before a single test runs.
- The domain-separation tags prefixed `dcrpoker/` and `gaming/table/` are frozen
  hash inputs. Never normalise them, however inconsistent they look: changing one
  invalidates live bonds, punishment keys and signed logs.
- `txscript/v4` and `wire` are pinned in `go.mod` and checked in CI. The escrow
  scripts build on those exact versions.
- `escrow.MaxMembers = 6` is a fact about the scripts, inherited by every game.

**Your own seat-key tags are yours to choose and yours to freeze.** You pass them
in `Config.SeatTags`; the SDK will not invent them, because they decide which
keys your seats have and changing one later strands whatever those keys held.
Pick them once, with a `/v1` suffix, and never touch them.

## What is not built yet

Honest state of the runtime, so you know what you would hit:

| Stage | State |
|---|---|
| connect, identify, dial | done |
| invite, terms, seating | done |
| spend book and its state machine | done |
| funding a stake | done |
| settlement: build, co-sign, broadcast | done |
| reclaiming a bond, a stake or a table bond | done |
| forfeiture mechanism | done (`pkg/punish`, `pkg/evidence`) |
| forfeiture *execution* through the runtime | **not built** |

The last row is the one gap, and it is a real one. Carrying out a forfeiture
needs the punishment-key exchange first - every seat announcing, with a proof of
possession, the key its opponent's forfeitable bond will name - and then those
bonds funded. The mechanism that spends them is here and tested; what is missing
is the exchange that sets them up. Until then `Forfeit` validates your ruling,
verifies an equivocation cryptographically, and then tells you the stage is not
built rather than guessing.

Anything unbuilt returns an error wrapping `runtime.ErrNotYet`, naming the stage.
Nothing guesses.

## Compatibility

The module is pre-1.0 and the runtime packages are new. Treat `pkg/runtime`,
`pkg/spend`, `pkg/ruling`, `pkg/gaming/connect` and `pkg/gaming/bridgetest` as
unstable until this section says otherwise.

The older packages - `escrow`, `forfeit`, `membership`, `gamelog`,
`gaming/{schema,wire,transport,gamingpb}` - carry live coin and change only
additively. `membership.Terms` gained bond fields without moving the digest of
a table that states none, and that is the standard the rest is held to.

## Another language

The module is Go. If you are not, what you need is the protocol rather than the
library: the gRPC service in `pkg/gaming/gamingpb`, the frame format in
`pkg/gaming/wire`, the message envelope in `pkg/gaming/schema`, and the scripts
in `pkg/escrow`. All four are documented in their package comments, which are
long on purpose.

Be aware that you would be reimplementing the money-safety rules on this page,
including the one that cost 0.01 DCR.
