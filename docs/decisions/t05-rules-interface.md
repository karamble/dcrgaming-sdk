# T-05 — The `Rules` interface: decision

**Spike outcome: PROCEED with four methods.** Both games satisfy the same
interface and neither needs a method the other does not.

Branch `runtime-t05-rules` · package `pkg/runtime` · 2026-08-19

---

## The question

Is the interface generic, or battleships-shaped? Designed against **poker
first**, deliberately: designing against battleships and validating against
battleships proves nothing.

**Decision rule as written:** poker needs a method battleships does not → the
interface is poker-shaped and the extra method is probably adjudication leaking
in. Poker fits with no additions → commit the shape.

## The answer

```go
type Rules interface {
	Identity() connect.Identity
	Terms(sid string) (membership.Terms, error)
	Handle(ctx context.Context, in Message) error
	State(ctx context.Context) State
}
```

Four methods, and each is one the runtime genuinely cannot answer: what this
game is called, what a table costs, what a message means, and what the operator
should be shown. Nothing that the runtime can work out for itself is in here.

Two things flow the other way, as methods on the runtime rather than on `Rules`,
because they happen when the game's rules say so and not when the runtime asks:
**an outcome** (the runtime does not know who won) and **a ruling** (the runtime
does not know who cheated).

## How it was checked

Both games' shapes are implemented against the interface, from their real
concepts, and both satisfy it as a compile-time fact:

```go
var (
	_ Rules   = (*pokerRules)(nil)
	_ Rules   = (*battleshipsRules)(nil)
)
```

They differ in every way the interface allows and in none it does not:

| | poker | battleships |
|---|---|---|
| refund lock | 288 blocks (hand-sized) | 2048 blocks |
| bond terms | none | 1,000,000 atoms / 4032 blocks |
| vocabulary | shuffle, share, cardkey, action, checkpoint, reveal | place, shoot, answer, punishkey, reveal |
| optional hooks | takes neither | takes both |

The vocabularies overlap on exactly one word, `reveal`, and it means unrelated
things in the two games - which is the point: the runtime never looks inside a
body, so the collision costs nothing.

**Neither game needed a method the other did not.** Poker's deep dispute
machinery - `challengeHand`, `judgeComplaint`, `proposeTake`,
`presignAccusations` - wanted no interface method, because it is adjudication
and adjudication stays with the game. That is the decision rule's warning
condition, and it did not fire.

## Two shapes chosen against the obvious one

**Outcome is shares, not a winner.** A draw, a split pot and a three-way are all
outcomes a game can reach; an interface that only understood "winner" would push
every game into pretending. `Void` is separate again, for a table that unwinds
rather than pays out - a game reaching an outcome nobody won says so rather than
inventing an even split.

**The optional hooks are optional.** `Seated` and `Settled` are separate
interfaces a game may implement. Battleships wants both; poker wants neither.
Folding them into `Rules` would have made poker implement two empty methods,
which is how a four-method interface becomes a six-method one nobody reads.

## The read surface, stated deliberately

Because duty clocks stay with the game, the runtime has to expose enough chain
and log for a game to run them - and no more. That is `Chain` (height and hash)
and `Log` (the signed entries). Notably absent: anything that builds or
broadcasts. A game that could broadcast could pay somebody.

## What this spike did not do

It did not port poker's real call sites. The interface is validated by
implementing both games' shapes against it, which is stronger than reading and
weaker than a port. **The port is T-14**, and it is the genuine proof - if
poker's real seating and settlement code needs something this interface does not
offer, T-14 is where that surfaces, and the wrong-condition in the PRD is
written to catch exactly that.

## Verification

`go vet ./pkg/runtime/` and `go test ./pkg/runtime/` exit 0. The interface
satisfaction is compile-time, so a shape either game could not meet would fail
the build rather than a test.

## Unblocks

**T-08** (the runtime skeleton implements this), and with it waves 4 and 5.
