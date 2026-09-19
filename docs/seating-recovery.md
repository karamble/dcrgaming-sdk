# Durable seating and payment recovery

This SDK is under development. Earlier games are design references,
not compatibility targets or evidence of production deployment.

## Runtime lifecycle

`runtime.Open` keeps both stores under its `Dir` and acquires exclusive
ownership of them; another runtime on the same directory is refused. It resumes
as it opens, so a caller inspects `Resumed()` for failed records and then starts
the event loop with `Run`, which follows the chain on its own. Cancel and await
`Run` before reopening the stores. Use `Close` if the runtime was opened but
never started.

Acceptance writes the invitation, group-chat ID and complete terms before
acknowledging it or asking for an admission bond. Pending admissions can resume
without a formation. An admission bond must match the derived script and amount before a join is
published, but it is published unconfirmed on purpose: making depth an
admission condition races a short registration window against the
confirmations themselves. Depth is a later readiness condition, checked before
the table is seated. A deadline that expires while approval or confirmation is
pending leaves a recovery record, not a late join. Identical invite retries are idempotent; conflicting
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

## Getting money back out

This runtime has no recovery API. It does not assemble, sign or broadcast a
transaction, so there is no transaction journal here and nothing to rebroadcast.
A `TableStore` implements `LoadTables`, `SaveTable` and `DropTable`, and that is
the whole interface.

Money that was locked and never settled is recovered by its owner through
dcrpulse, under Gaming then Recovery, once the timelock matures. That covers a
stake at a table that never paid out and an admission bond at a table that never
formed. The game has no part in it.

A seated table that is not fully funded by `membership.FundingDeadline(terms)`
is marked recovery-only and its formation abandoned. The membership is kept, so
every seat's refund script stays derivable; the money still comes back the same
way, after `Terms.CSVBlocks`.

The two deposit purposes are `seatbond` and `stake`. There is no table bond and
no forfeitable bond; terms asking for one are refused.
