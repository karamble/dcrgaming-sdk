# Hosting a gaming bridge

You have a wallet app - a node, a wallet, a Bison Relay client and a person
who owns them - and you want that person to be able to put games on it. This
is what your app provides, what it has to do, and what it must never do.

dcrpulse is the reference host: everything below is what
`dashboard/internal/services/gaming_host.go` and `gaming_intake.go` there do.

---

## What the bridge is

`pkg/gaming/bridge` runs inside your app's process. Games are separate
programs, run wherever their operator likes, and they connect in to the
bridge's own port with a certificate the operator issued them. A game can ask
the bridge to move money; it can never move money itself.

The bridge holds no keys. When a payment is due it asks your wallet to build
it, shows it to the operator, and asks your wallet to sign it only once the
operator approved that exact transaction with their passphrase.

Three rules hold whatever the host:

1. **A game reaches only the bridge's contract.** Everything your app hands
   the bridge is an in-process call. Nothing of your wallet, node or Bison
   Relay client is reachable from the port.
2. **Every payment passes the whole chain.** The game's credential, the
   account and caps the operator bound for that game, a pending request, the
   operator approving that exact transaction with their passphrase, your wallet
   signing it, and only then publishing.
3. **The operator is protected.** While your operator reports that their
   console is unprotected, the port is closed and every call is refused.

## Running one

```go
bridge.UseLogger(log)
listener.UseLogger(log)

b := bridge.New(dataDir, bridge.Host{
	Node:     func() bridge.Chain { return node }, // nil while there is none
	Wallet:   wallet,
	Relay:    relay,
	Operator: operator,
})

// Start feeding group messages (below), then:
go b.RecoverHistory()
if err := b.Start("0.0.0.0:8443"); err != nil {
	log.Errorf("%v", err) // gaming is one section of your app; keep the rest running
}
defer b.Stop()
```

`New` does nothing on its own. `Start` opens the port and the financial worker
that reconciles payments with the chain; `Stop` closes both. `RecoverHistory`
replays frames already journaled into the tables they belong to, and costs
nothing when there are none.

## What your app provides

A part you leave nil reads as unavailable, never as trusted: no node means no
chain reads, no operator means an unprotected one.

### Chain

The node. Its methods are those of dcrd's `*rpcclient.Client`, so you can hand
one over as it is.

- **dcrd must run with `txindex=1`.** A payout every seat signed is broadcast
  by a reconcile pass that first looks it up, and without the index that lookup
  fails in a way that cannot be told from "not found". When the operator saves
  the settings, pass whether they are protected and `b.TxIndexActive(ctx)` to
  `WriteGamingSettings`; the bridge will not switch on without both.
- The network name comes from `getblockchaininfo` and is kept once known.

### Wallet

Custody of the operator's coins. Each method applies your wallet's own guards
and fails rather than act on a wallet it cannot vouch for.

| Method | What it must do |
|---|---|
| `CheckSigning` | Fail unless the open wallet can sign. A watching-only wallet, or one whose kind you do not know, holds no gaming funds. |
| `Accounts`, `ReservedAccount` | List accounts; report the names your app keeps for itself (mixing, Lightning, trading, imported). A game is never bound to a reserved one. |
| `AccountXPub` | The account's extended public key. It fingerprints the wallet, so the ledger knows which wallet its deposits belong to. |
| `ValidateAddress` | Whether an address is the wallet's, in which account, and its public key. |
| `NextInternalAddress` | A fresh key per table, and an error rather than wrapping around to a key handed out before. |
| `ImportScript` | Watch an escrow script, so the wallet sees the coins in it. |
| `Construct` | Build, unsigned, a payment of an amount to an address from an account, with your change policy. Change must stay in that account. |
| `SignTransaction` | Sign that transaction with the operator's passphrase. |
| `WithUnlockedAccount`, `SignHash` | Unlock an account for a call and relock it only if you unlocked it; sign a 32-byte hash, DER-encoded, with the key behind an address. |
| `Publish`, `Broadcast` | Relay a transaction. `Publish` may have reached the network even when it errs; `Broadcast` returns your wallet's own error, so a transient failure can be told from a refusal. |
| `Transaction`, `MinedTransactions` | One of the wallet's transactions, or none; the wallet's mined transactions block by block from a height. |

### Relay

Your Bison Relay client.

- `Identity` is your user id and the nick Bison Relay logs your own messages
  under.
- `SendGroupMessage` posts to a group chat. Return an error matching
  `bridge.ErrNotSent` only when Bison Relay certainly did not take the
  message; any other error leaves that unknown, and the bridge settles it from
  `GroupHistory` rather than sending twice.
- `GroupHistory` is Bison Relay's own log of a group chat, a page at a time.

### Operator

The person the bridge answers to. `Protected` reports that their console is
behind a password; `PresenceChanged` says a game connected or went away, for
your console to redraw.

## What your app must do

- **Hand the bridge every group message, at least once, with its sender.**
  Call `b.ReceiveGroupMessage(bridge.GroupMessage{GCID, From, Text, Time})`
  with the sender's authenticated user id, and acknowledge the message to its
  source only when that returns nil: an error means the frame was not kept and
  has to come again. With a Bison Relay client this is `ChatService.GCMStream`
  and `AckReceivedGCM`, and the bridge must be its only acker. A player whose
  frames are lost looks exactly like a player who walked away, and is
  penalised as one.
- **Keep frames out of chat.** Drop anything `wire.IsEnvelope` reports from
  chat views and notifications, and refuse a content filter that would match
  `wire.SampleEnvelope`: such a filter would drop players' moves before the
  bridge sees them.
- **Give the operator the console.** Registering games and binding accounts
  (`WriteGamingSettings`), issuing a game's credential (`IssueGamingCredential`,
  whose key is shown once and stored nowhere), approving and denying payments
  (`ApproveGamingSpend`, `DenyGamingSpend`), payouts (`ApproveGamingPayout`,
  `RejectGamingPayout`), tables (`CreateGamingTable`, `AcceptGamingInvite`),
  and recovery (`GamingRecoveryList`, `QuoteGamingRecovery`,
  `ConfirmGamingRecovery`, `GamingLedgerBackup`). Every approval takes the
  operator's passphrase, and the bridge wipes it.
- **Never hand a game anything else.** No daemon credentials, no wallet
  access, no second port. What a game may do is exactly the bridge's contract.

`ErrGamingNeedsAppPassword` still names dcrpulse's App Password in its text.
Match it with `errors.Is` and show your operator your own words.

## The data directory

Everything the bridge keeps lives in the directory you give `New`. Keep it, and
do not move or rename what is in it: a bridge that looks somewhere new finds no
registered games and no ledger, and every game it knew becomes a stranger.

| Path | What it holds |
|---|---|
| `gaming.json` | Registered games, their bound accounts and caps, and the certificates they connect with. |
| `gaming-spends.json` | Every payment a game asked for; the daily cap counts from it. |
| `financial-authority/` | The ledger: tables, deposits, escrows, payouts. Back it up with `GamingLedgerBackup`. |
| `gaming-frames.jsonl` | Every frame received, with its sender, until its tables settle. |
| `gaming-wire-inbox-v2.jsonl` | Frames delivered to games, so a game that reconnects resumes. |
| `gaming-wire-outbox-v2.jsonl` | What was sent, so nothing is ever sent twice. |
| `gaming-bridge.cert`, `gaming-bridge.key` | The bridge's own identity. Games pin it; a new one breaks every connected game. |

## Testing a host

`pkg/gaming/bridge/conformance_test.go` runs the real bridge and listener
against a fake host in `fakehost_test.go`, driven by the same game client every
game links: frames both ways, chain reads, a payment that waits for the
operator, one that is denied, and one over a cap. The two-bridge test in
`conformance_table_test.go` plays a whole table - bonds, seating, stakes and a
co-signed payout - between two hosts on one chain. The fake host is the
smallest thing that behaves like a real one; where your adapter differs from
it, the difference is worth a test of its own.
