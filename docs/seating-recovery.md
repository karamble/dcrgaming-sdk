# Durable seating and payment recovery

This SDK is under development. Poker and Battleships are design references,
not compatibility targets or evidence of production deployment.

## Runtime lifecycle

Use `spend.FileStore` and `runtime.NewFileTableStore` for persisted state.
`runtime.New` acquires exclusive ownership of these file-backed stores; another
runtime using either store is refused. Call `ResumeWithReport` before `Run`,
inspect failed records, then run the event loop and call `Tick` as chain heights
advance. Cancel and await `Run` before reopening stores. Use `Close` if the
runtime was constructed but never started.

Acceptance writes the invitation, group-chat ID and complete terms before
acknowledging it or asking for an admission bond. Pending admissions can resume
without a formation. A per-table bond must match the derived script and amount
and reach the required confirmations before a join is published. A deadline
that expires while approval or confirmation is pending leaves a recovery
record, not a late join. Identical invite retries are idempotent; conflicting
terms or chat IDs for the same session are refused.

Storage errors latch a fault. Further financial operations stop. Repair storage
and reopen from durable records; do not clear errors in memory and continue.

## Payment obligations

One immutable obligation binds table terms, the derived session identity,
purpose, script, address and amount. Concurrent callers share its recorded
request. A request intent with no bridge ID means dispatch is unresolved. It
must **not** automatically issue another payment.

`ReconcileSpend(ctx, match, purpose, id)` checks an operator-supplied ID against
the bridge's authoritative game, address, amount and purpose before adopting
it. The current bridge has no request lookup by client obligation ID, so a lost
ID cannot be recovered automatically by the SDK alone.

`RetryFunding(ctx, id)` permits an explicit new attempt only after a fresh
bridge response proves the previous request denied or expired without a
transaction. Prior attempts remain recorded. An ambiguous or failed request
cannot be treated as proof that no payment occurred.

## UI and game policy

An optional `InviteResolver` receives the merged invite terms. Use it to derive
game policy such as a bond scaled to the advertised stake. The resolver cannot
replace advertised stake, seats, deadline or refund lock.

`Snapshot` returns detached table, seating, deposit and payment data.
`RefreshDeposits` independently checks known outputs, scripts, amounts and
confirmations. `CheckAdmissionBonds` checks the complete roster's admission
outputs. A `seated` phase reports agreement on the roster and seat order; it is
not permission to start paid gameplay. Games must check admission bonds, all
required stake/bond deposits, and their own readiness protocol before play.

`Bridge.ConnectionStatus` distinguishes `connecting`, `subscribed`,
`reconnecting` and `stopped`. Subscription is reported only after `StreamStart`.
Notifications coalesce and cannot block the bridge event loop.

## Recovery transactions

The table store also implements `OperationStore`. Custom stores must provide
that interface or configure a separate journal. Every SDK broadcast is preceded
by a durable record of its exact signed bytes and transaction ID.
`PreparedTransactions` exposes that journal. `RetryTransaction` rebroadcasts
only the recorded transaction after checking its ID; journal presence alone
does not mean it was mined.

`ReclaimSeatBond` refunds a matured per-table admission bond using the original
terms and derived key, including a table that never formed. Existing stake,
table-bond and forfeitable-bond recovery APIs remain separate.

No offline-consensus policy is inferred by this work. Accusation ladders are
heads-up mechanisms; a multiplayer game must define and verify its own rules.
