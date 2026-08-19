// Package spend is the record of money a game has asked the bridge to move,
// and the state machine that decides what each answer means.
//
// It exists because both games that reached mainnet wrote this themselves, with
// incompatible records - one keyed by session id and seat, the other by match;
// one storing an outpoint, the other a vout; one keeping a field for "could not
// ask" and the other not - and because one of them learned the hard way what
// happens when the distinction below is lost.
//
// # The distinction the whole package is built around
//
// There are two ways to fail to get a yes, and treating them alike costs money:
//
//   - The bridge answered, and the answer was no. Terminal. The money did not
//     move and never will.
//   - This process could not ask. Not an answer at all. The bridge may have paid
//     while the question was in flight - a restart of the bridge is exactly when
//     that happens - so the request is still open and must be asked about again.
//
// dcrpoker's source carries the receipt for getting this wrong: "Recording it as
// refused here once cost a real 0.01 DCR: the stake was paid, this stopped
// watching, the interface still said it was owed, and it was paid a second
// time."
//
// So Unknown is a state, not an error return. A request in it is open, is
// retried, and survives a restart still open. Nothing in this package can move a
// request out of open because a call failed; only an answer does that, and
// [Record.Note] refuses to be handed one that did not come from the bridge.
//
// # What is not here
//
// No policy. The bridge decides whether a game may spend and asks a person to
// approve it; this only remembers what was asked and what came back. And no
// transaction building - that is escrow's, and a record names an address and an
// amount, never a script it made up.
package spend

import "fmt"

// State is where a request stands.
//
// The zero value is deliberately not a valid state: a record that was never
// asked for should not read as one that was requested.
type State string

const (
	// Requested has been asked and nobody has answered.
	Requested State = "requested"

	// Unknown means this process could not ask. It is not an answer, and a
	// request in it is still open. See the package comment.
	Unknown State = "unknown"

	// Approved is the bridge's yes, and carries a txid.
	Approved State = "approved"

	// Located is Approved and the output has been found on chain. This is
	// the only state in which a game may rely on the money being there.
	Located State = "located"

	// Denied is the bridge's no. Terminal.
	Denied State = "denied"

	// Failed is the bridge trying and not managing it. Terminal.
	Failed State = "failed"

	// Expired is the request timing out unanswered. Terminal.
	Expired State = "expired"
)

// Open reports whether the request may still move money, and so must still be
// asked about.
//
// Unknown is open, which is the point: not knowing is not a reason to stop
// watching.
func (s State) Open() bool {
	switch s {
	case Requested, Unknown, Approved:
		return true
	}
	return false
}

// Terminal reports whether nothing further can happen to the request.
func (s State) Terminal() bool { return s.valid() && !s.Open() }

func (s State) valid() bool {
	switch s {
	case Requested, Unknown, Approved, Located, Denied, Failed, Expired:
		return true
	}
	return false
}

// canFollow reports whether one state may follow another.
//
// The rules are few and each is here to stop a specific way of losing money:
//
//   - Nothing leaves a terminal state. A denied request that later reads as
//     approved would be paid twice.
//   - Unknown may be entered from any open state and left for any state. Not
//     knowing is a suspension of knowledge, not a finding, so it neither closes
//     anything nor prevents anything.
//   - Located may only follow Approved. An output cannot be found for a payment
//     nobody agreed to make.
func canFollow(from, to State) error {
	if !to.valid() {
		return fmt.Errorf("%q is not a state a request can be in", to)
	}
	if from == to {
		return nil
	}
	if from.Terminal() {
		return fmt.Errorf("a request that is already %s cannot become %s", from, to)
	}
	if to == Located && from != Approved {
		return fmt.Errorf("a request cannot be located from %s; only an approved payment has an output", from)
	}
	return nil
}
