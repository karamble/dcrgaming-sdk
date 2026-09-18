# dcrgaming-sdk

The game-agnostic layer for trustless money games on Decred.

A game built on it gets a table whose money nobody holds alone: every buy-in
sits in an escrow that needs every seat's signature to move, and each player
can always recover their own stake alone after a timelock. A player who signs
two different statements at the same position publishes their own key and
loses a bond for it. Every action is signed and hash-chained, so the whole
game is a log anyone at the table can audit and nobody can rewrite.

What lives here is exactly the part that does not care which game is being
played: the escrow scripts and their proofs of possession, the forfeit keys
that make equivocation self-punishing, the membership protocol that forms a
roster and pins its terms, the tamper-evident game log, the message schema
and its wire framing, and the gRPC proto plus transport a game uses to reach
a dcrpulse gaming bridge. The referee, the cards and everything else that
knows the rules of a particular game stays with that game.

Building a game on it? Start with
**[docs/building-a-game.md](docs/building-a-game.md)** - what you write, what you
do not, and the one failure mode that costs money.

## Reading it

- `pkg/runtime` - the lifecycle a game plugs into. It owns the loop; a game
  implements four methods and never writes a bridge dispatcher, a spend book or
  a seating machine.
- `pkg/spend` - the record of money in flight, and the state machine that keeps
  "could not ask" apart from "the answer was no".
- `pkg/evidence` - both halves of an equivocation, retained, and the key they
  give up. Retaining it is all that happens here: this module carries no
  forfeiture out.
- `pkg/gaming/connect` - a game's connection to a bridge, headless.
- `pkg/gaming/bridgetest` - a bridge that exists only in your process, with the
  failures a game has to survive.
- `pkg/escrow` - the money: multisig escrow scripts, addresses and bond
  proofs of possession.
- `pkg/forfeit` - why cheating publishes your key: aggregated forfeit keys
  and position-derived nonces.
- `pkg/membership` - how a table forms: invites, terms, roster and the match
  id both sides must agree on.
- `pkg/gamelog` - a signed, hash-chained log a game may replay from. Offered to
  games; `pkg/runtime` does not read or replay it.
- `pkg/gaming/schema` - the message envelope, formation, invite, liveness and
  duty vocabulary shared by every game.
- `pkg/gaming/wire` - how a message is framed for a Bison Relay group chat.
- `pkg/gaming/transport` and `pkg/gaming/gamingpb` - the bridge proto and the
  client that dials one dcrpulse gaming bridge and reaches nothing else.

Package comments carry the reasoning, and they are long deliberately.

## Status

This SDK is under development. The full money path - bond, seat draw, stake,
play, cooperative payout - runs end to end against two independent wallets and
two bridges on simnet. The warts below are deliberate; every one is load-bearing
under live coin.

- The proto package is `dcrpulse.gaming.v2` and its major is frozen: it is
  baked into every gRPC `:path` a live bridge routes on, so a game and the
  bridge it dials must share it.
- The descriptor's recorded source path is the bare `gaming_bridge.proto`.
  No generator ships here and none may be added: dcrpulse is the generator
  of record, and any regen must keep `--proto_path` pointed at the gamingpb
  directory itself or the descriptor bytes change. The toolchain of record
  pins `protoc-gen-go v1.36.11` and `protoc-gen-go-grpc v1.6.2`.
- A binary links this module's gamingpb OR a vendored copy, never both.
  The descriptor registers at init and protobuf's registry policy is panic
  on conflict, so double registration is a crash before a single test runs.
- The `dcrpoker/`- and `gaming/table/`-prefixed domain-separation tags are
  frozen hash inputs. They are never to be normalized, however inconsistent
  they look: changing one invalidates live bonds, punishment keys and
  signed logs.
- A game introduces itself through the transport and schema configuration; it
  does not edit the shared vocabulary to do it.
- `schema.Duty` and `schema.DutyKind` carry a card game's vocabulary -
  cardkey, shuffle, share, action, checkpoint, reveal, indexed by hand - and so
  does half of `forfeit.Domain`. A game names its own duties; it cannot add a
  `forfeit.Domain`, which is a closed allowlist of nine values.
- `identity.Credentials` derives all three seat keys per session id, but
  `identity.BondDeposit` is one outpoint per identity. Pair them and the bond
  key opens nothing. `pkg/runtime` derives the bond key with no session id, and
  a direct caller has to do the same.
- Nothing in the schema package, tests included, may import a game's
  driver: once a driver imports schema, that is an import cycle.
- `escrow.MaxMembers = 13` is an escrow-script fact - the redeem-script push
  limit, not a seat count anybody chose - inherited by every game.
- `txscript/v4 v4.1.1` and `wire v1.7.0` are pinned in go.mod and checked
  in CI; the escrow scripts build on these exact versions and a bump
  changes bytes guarding live mainnet bonds.

## License

ISC - see the LICENSE file.
