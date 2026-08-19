package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/punish"
)

// KindAccusation carries a signature on one rung of an accusation chain. The
// runtime's own.
const KindAccusation = schema.KindAccusation

// ladder is the pre-signed chain of accusations against one seat's table bond.
//
// Built and co-signed at bonding, before anybody has misbehaved, because a rung
// spends the bond's cooperative branch and needs both signatures - and a seat
// that has gone silent will not be signing anything. Pre-signing is what turns
// "my opponent stopped answering" from a stalemate into something the wronged
// seat can act on alone.
//
// The chain's later outpoints are fixed in advance because a Decred
// transaction's identity excludes its witness, so rung two can be built against
// rung one before rung one has a signature on it.
type ladder struct {
	// against is the seat whose bond these rungs spend, and bond is its
	// script.
	against uint32
	bond    []byte
	rungs   []*wire.MsgTx
	// sigs[i] is the signatures gathered on rung i, by signer pubkey hex.
	sigs []map[string][]byte
	// ready[i] is rung i once both seats have signed it.
	ready []*wire.MsgTx
	// run is how many rungs have been broadcast.
	run int
}

// PresignLadder builds the accusation chain against the other seat's table bond
// and signs every rung, then tells the table so it can do the same.
//
// Called at bonding. Leaving it until an accusation is needed would be leaving
// it until the seat whose signature is required has stopped answering.
func (r *Runtime) PresignLadder(ctx context.Context, match string) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	seats, _ := t.form.Seats()
	if len(seats) != 2 {
		return fmt.Errorf("an accusation chain is heads-up, and this table seats %d", len(seats))
	}
	session, _, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return err
	}
	mineHex := hex.EncodeToString(seats[mine])

	// Both chains, and that is the whole of why this works: a rung needs
	// every member's signature including the accused's, so each seat has to
	// sign the chain that can be run against itself. The two sides build
	// the same bytes for a given target - the draft comes from that seat's
	// bond, its outpoint, the agreed fee and the chain - so signatures can
	// be exchanged by matching the transaction rather than by trusting an
	// index or a description of it.
	//
	// Building only the chain against the opponent leaves each side holding
	// transactions the other has never seen, and no rung is ever co-signed.
	for _, against := range []uint32{mine, 1 - mine} {
		if err := r.presignAgainst(ctx, t, against, mineHex, session); err != nil {
			return err
		}
	}
	return nil
}

// presignAgainst builds, signs and announces the chain against one seat.
func (r *Runtime) presignAgainst(ctx context.Context, t *table, against uint32,
	mineHex string, session *secp256k1.PrivateKey) error {

	draft, err := r.accuseDraft(t, against)
	if err != nil {
		return err
	}
	rungs, err := punish.BuildLadder(draft)
	if err != nil {
		return fmt.Errorf("build the accusation chain against seat %d: %w", against, err)
	}

	l := &ladder{against: against, bond: draft.Bond, rungs: rungs}
	l.sigs = make([]map[string][]byte, len(rungs))
	l.ready = make([]*wire.MsgTx, len(rungs))
	type announce struct{ raw, sig string }
	out := make([]announce, 0, len(rungs))
	for i, tx := range rungs {
		sig, err := escrow.SignBondSpend(tx, draft.Bond, session)
		if err != nil {
			return fmt.Errorf("sign rung %d against seat %d: %w", i, against, err)
		}
		l.sigs[i] = map[string][]byte{mineHex: sig}
		raw, err := tx.Bytes()
		if err != nil {
			return err
		}
		out = append(out, announce{raw: hex.EncodeToString(raw), sig: hex.EncodeToString(sig)})
	}

	r.mu.Lock()
	if t.ladders == nil {
		t.ladders = map[uint32]*ladder{}
	}
	if held, seen := t.ladders[against]; seen {
		// Already built. Keep the signatures already gathered rather
		// than starting the exchange again with an empty set.
		l = held
	} else {
		t.ladders[against] = l
	}
	r.mu.Unlock()

	// One message per rung, each carrying the transaction it signs, so the
	// other side matches it against the chain it built rather than trusting
	// an index.
	for i, a := range out {
		if err := r.send(ctx, t, KindAccusation, schema.Accusation{
			Seat: against, Tx: a.raw, Signer: mineHex, Sig: a.sig,
		}); err != nil {
			return fmt.Errorf("announce rung %d against seat %d: %w", i, against, err)
		}
	}
	return nil
}

// accuseDraft is the chain's first rung against a seat's table bond.
func (r *Runtime) accuseDraft(t *table, against uint32) (escrow.AccuseDraft, error) {
	// Checked before anything is looked up: the fee comes from the terms and
	// the game, not from the chain, so a table that has neither is malformed
	// rather than merely unfunded.
	fee := feeFor(t.form.Terms().AccuseFeeAtoms, r.accuseFee)
	if fee <= 0 {
		return escrow.AccuseDraft{}, fmt.Errorf(
			"neither this table's terms nor this game states what one rung of an " +
				"accusation costs, and a chain built at a fee nobody chose has an " +
				"attrition bound nobody can rely on")
	}
	bond, err := r.tableBondOf(t, against)
	if err != nil {
		return escrow.AccuseDraft{}, err
	}
	script, err := hex.DecodeString(bond.ScriptHex)
	if err != nil || len(script) == 0 {
		return escrow.AccuseDraft{}, fmt.Errorf("seat %d's table bond has no script", against)
	}
	r.mu.Lock()
	funded, ok := t.tableBondFunded[against]
	r.mu.Unlock()
	if !ok || funded.outpoint == "" {
		return escrow.AccuseDraft{}, fmt.Errorf("seat %d's table bond is not on the chain", against)
	}
	prevout, err := outpointOf(funded.outpoint)
	if err != nil {
		return escrow.AccuseDraft{}, err
	}
	return escrow.AccuseDraft{
		Bond: script, Prevout: prevout, ValueAtoms: funded.atoms,
		FeeAtoms: fee, Params: r.params,
	}, nil
}

// feeFor is the accusation fee's precedence: the table's terms where they state
// one, the game's own otherwise, and nothing when neither does.
//
// Terms win because a fee both seats agreed is stronger than one they each read
// from their own build. The fallback exists for tables formed before the term
// did, whose digest cannot move to add it.
func feeFor(stated, game uint64) int64 {
	if stated > 0 {
		return int64(stated)
	}
	return int64(game)
}

// adoptAccusation takes the other seat's signature on one rung.
//
// The rung is matched against the chain this peer built itself, by its bytes.
// A seat that could get the other to sign a rung it had not computed could
// pre-sign a chain draining the bond somewhere else, and would only need it
// signed once.
func (r *Runtime) adoptAccusation(ctx context.Context, match string, body schema.Accusation) error {
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	seats, ok := t.form.Seats()
	if !ok {
		return fmt.Errorf("this table has no seating yet")
	}
	signer, err := hex.DecodeString(body.Signer)
	if err != nil || !seatedKey(seats, signer) {
		return fmt.Errorf("an accusation chain was signed by somebody who is not at this table")
	}

	sig, err := hex.DecodeString(body.Sig)
	if err != nil || len(sig) == 0 {
		return fmt.Errorf("that rung's signature is not usable")
	}

	r.mu.Lock()
	l := t.ladders[body.Seat]
	if l == nil {
		r.mu.Unlock()
		return fmt.Errorf("no accusation chain against seat %d has been built at this table yet",
			body.Seat)
	}
	at := -1
	for i, tx := range l.rungs {
		raw, err := tx.Bytes()
		if err != nil {
			r.mu.Unlock()
			return err
		}
		if hex.EncodeToString(raw) == body.Tx {
			at = i
			break
		}
	}
	if at < 0 {
		r.mu.Unlock()
		return fmt.Errorf("that is not a rung of the chain this peer built, and it was not signed")
	}
	// Already finished here, which means the other side is still short of
	// our signature: nobody repeats a rung they have completed. Answering
	// one cannot echo, because the answer completes them and they stop -
	// and it is the only acknowledgement this exchange has. Without it the
	// peer that finishes first falls silent, and the peer that missed its
	// first send waits for a message that will never come again.
	wasReady := l.ready[at] != nil
	l.sigs[at][body.Signer] = sig
	if err := l.finish(l.bond, r.params); err != nil {
		r.mu.Unlock()
		return err
	}
	mine, seated := t.form.OurSeat()
	var answer []byte
	if wasReady && seated {
		answer = l.sigs[at][hex.EncodeToString(seats[mine])]
	}
	against := l.against
	r.mu.Unlock()

	if len(answer) == 0 {
		return nil
	}
	return r.send(ctx, t, KindAccusation, schema.Accusation{
		Seat: against, Tx: body.Tx, Signer: hex.EncodeToString(seats[mine]),
		Sig: hex.EncodeToString(answer),
	})
}

// finish co-signs every rung both seats have signed. Caller holds the lock.
//
// Signatures go in canonical member order, which is the order escrow.Members
// reports for the bond, because that is the order the script pops them in.
func (l *ladder) finish(bond []byte, params stdaddr.AddressParams) error {
	members, err := escrow.Members(bond)
	if err != nil {
		return err
	}
	for i, tx := range l.rungs {
		if l.ready[i] != nil {
			continue
		}
		ordered := make([][]byte, 0, len(members))
		for _, m := range members {
			sig, ok := l.sigs[i][hex.EncodeToString(m)]
			if !ok {
				return nil // this rung is still short of somebody
			}
			ordered = append(ordered, sig)
		}
		done, err := punish.CoSignAccuse(tx, bond, ordered, params)
		if err != nil {
			return fmt.Errorf("rung %d does not satisfy the bond: %w", i, err)
		}
		l.ready[i] = done
	}
	return nil
}

// RunLadder broadcasts the next accusation against a seat that has stopped
// answering.
//
// One rung at a time, because each is answerable: the accused spends the
// claimed bond straight back and the chain moves on. Running them all at once
// would spend the bond into a chain nobody can answer.
func (r *Runtime) runLadder(ctx context.Context, match string, against uint32) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	// Both chains are held here, and only one of them is this seat's to
	// run. The other exists so the opponent can run it at this seat, and
	// running it here would be spending our own bond into a claim the
	// opponent then answers and keeps.
	if against == mine {
		return fmt.Errorf("the chain against seat %d is this seat's own, and is not ours to run", against)
	}
	r.mu.Lock()
	l := t.ladders[against]
	r.mu.Unlock()
	if l == nil {
		return fmt.Errorf("no accusation chain against seat %d has been built at this table", against)
	}
	if l.against != against {
		return fmt.Errorf("the chain at this table is against seat %d, not %d", l.against, against)
	}

	r.mu.Lock()
	next := l.run
	var tx *wire.MsgTx
	if next < len(l.ready) {
		tx = l.ready[next]
	}
	r.mu.Unlock()
	if tx == nil {
		if next >= len(l.ready) {
			return fmt.Errorf("every rung has been run; the bond is spent down to its attrition bound")
		}
		return fmt.Errorf("rung %d is not co-signed yet", next)
	}

	raw, err := tx.Bytes()
	if err != nil {
		return err
	}
	txid, err := r.bridge.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		return fmt.Errorf("send rung %d: %w", next, err)
	}
	r.mu.Lock()
	l.run++
	r.mu.Unlock()
	r.log.Infof("table %s: accusation %d against seat %d sent in %s", match, next, against, txid)
	return nil
}

// Ladder reports how many rungs are ready and how many have been run.
func (r *Runtime) Ladder(match string) (ready, run int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, found := r.tables[match]
	if !found || t.form == nil {
		return 0, 0, false
	}
	mine, seated := t.form.OurSeat()
	if !seated {
		return 0, 0, false
	}
	// The chain against the opponent, which is the one this peer could run.
	// The chain against itself is not this peer's to count: it exists so
	// the opponent can run it.
	l := t.ladders[1-mine]
	if l == nil {
		return 0, 0, false
	}
	for _, tx := range l.ready {
		if tx != nil {
			ready++
		}
	}
	return ready, l.run, true
}

// AnswerAccusation is the accused seat replying to a rung: the claimed bond
// straight back into its own table bond.
//
// Signed alone, and that is the point. An accusation is not a verdict; it is a
// question the accused can answer for the cost of one fee. A seat that is alive
// and answering loses only attrition, which is what makes a false accusation
// worthless rather than dangerous.
func (r *Runtime) AnswerAccusation(ctx context.Context, match, claimedOutpoint string) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	if r.isSweeping(claimedOutpoint) {
		return fmt.Errorf("an answer to %s is already on its way from this process", claimedOutpoint)
	}
	draft, err := r.accuseDraft(t, mine)
	if err != nil {
		return err
	}
	claimed, err := draft.ClaimedScript()
	if err != nil {
		return fmt.Errorf("derive the claimed bond: %w", err)
	}
	prevout, value, err := r.claimedOutput(ctx, claimedOutpoint, claimed)
	if err != nil {
		return err
	}
	session, _, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return err
	}
	tx, err := punish.AnswerClaim(session, escrow.AnswerDraft{
		Claimed: claimed, Bond: draft.Bond, Prevout: prevout,
		ValueAtoms: value, FeeAtoms: draft.FeeAtoms, Params: r.params,
	})
	if err != nil {
		return fmt.Errorf("build the answer: %w", err)
	}
	return r.sendPunishment(ctx, tx, claimedOutpoint, "answer")
}

// TakeExpiredClaim takes a rung the accused never answered.
//
// The window is short and on chain, so "never answered" is a fact both sides
// read the same way rather than an opinion. Pays only the pinned payout.
func (r *Runtime) TakeExpiredClaim(ctx context.Context, match, claimedOutpoint string) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	if r.isSweeping(claimedOutpoint) {
		return fmt.Errorf("a take of %s is already on its way from this process", claimedOutpoint)
	}
	// The chain against the opponent, which is the only one this peer could
	// be taking from. The one against itself exists to be run at it.
	r.mu.Lock()
	l := t.ladders[1-mine]
	r.mu.Unlock()
	if l == nil {
		return fmt.Errorf("no accusation chain against seat %d has been built at this table", 1-mine)
	}
	if l.against == mine {
		return fmt.Errorf("this seat cannot take a claim against itself")
	}
	draft, err := r.accuseDraft(t, l.against)
	if err != nil {
		return err
	}
	claimed, err := draft.ClaimedScript()
	if err != nil {
		return fmt.Errorf("derive the claimed bond: %w", err)
	}
	prevout, value, err := r.claimedOutput(ctx, claimedOutpoint, claimed)
	if err != nil {
		return err
	}
	pinned, err := r.pinnedPayout()
	if err != nil {
		return err
	}
	session, _, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return err
	}
	tx, err := punish.TakeExpired(session, pinned, punish.Take{
		Claimed: claimed, Prevout: prevout, ValueAtoms: value,
		FeeAtoms: draft.FeeAtoms, Params: r.params,
	})
	if err != nil {
		return fmt.Errorf("build the take: %w", err)
	}
	return r.sendPunishment(ctx, tx, claimedOutpoint, "take")
}

// claimedOutput reads a claimed bond off the chain and checks it really is one.
func (r *Runtime) claimedOutput(ctx context.Context, outpoint string, claimed []byte) (wire.OutPoint, int64, error) {
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return wire.OutPoint{}, 0, err
	}
	out, err := r.bridge.UnconfirmedOutpoint(ctx, txid, vout)
	if err != nil {
		return wire.OutPoint{}, 0, err
	}
	if !out.Found {
		return wire.OutPoint{}, 0, fmt.Errorf("%s holds no coin", outpoint)
	}
	_, want, err := escrow.Address(claimed, r.params)
	if err != nil {
		return wire.OutPoint{}, 0, err
	}
	if got := hex.EncodeToString(want); !strings.EqualFold(got, out.PkScriptHex) {
		return wire.OutPoint{}, 0, fmt.Errorf(
			"%s pays %s and the claimed bond derives %s, so this is not a rung of this chain; "+
				"nothing was signed", outpoint, out.PkScriptHex, got)
	}
	prevout, err := outpointOf(outpoint)
	if err != nil {
		return wire.OutPoint{}, 0, err
	}
	return prevout, out.ValueAtoms, nil
}

// sendPunishment broadcasts one punishment spend and remembers it, so the same
// output is not spent twice from this process.
func (r *Runtime) sendPunishment(ctx context.Context, tx *wire.MsgTx, outpoint, what string) error {
	raw, err := tx.Bytes()
	if err != nil {
		return err
	}
	txid, err := r.bridge.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		return fmt.Errorf("send the %s: %w", what, err)
	}
	r.noteSweeping(outpoint)
	r.log.Infof("%s of %s sent in %s", what, outpoint, txid)
	return nil
}

// answerAnyClaim answers an accusation standing against this seat's own bond.
//
// Run every block, because not answering is what costs the bond. A rung spends
// the accused's table bond into a coin the accused can take straight back for
// one fee, and the whole design rests on that being cheap and automatic: a
// seat that has to notice a claim by hand loses its bond the first time it is
// asleep, and an accusation nobody answers is a false one that pays.
//
// The claim does not have to be discovered. Both seats hold both chains, so
// the accused already knows the exact transaction each rung is - it signed
// them - and only has to look on chain for whether one of them is there.
//
// Every rung is looked at rather than just the next one, and there is no need
// to stop at the first: a rung that is not on chain is not a claim, so this
// answers exactly what is standing and nothing else. Ordinarily that is one,
// because an accuser runs them one at a time.
func (r *Runtime) answerAnyClaim(ctx context.Context, t *table) {
	mine, seated := t.form.OurSeat()
	if !seated {
		return
	}
	r.mu.Lock()
	l := t.ladders[mine]
	r.mu.Unlock()
	if l == nil {
		return
	}
	for _, rung := range l.rungs {
		outpoint := rung.TxHash().String() + ":0"
		if r.isSweeping(outpoint) {
			continue
		}
		out, err := r.bridge.Outpoint(ctx, rung.TxHash().String(), 0)
		if err != nil || !out.Found {
			// Not on chain, or already answered and gone. Either way
			// there is nothing standing against this seat here.
			continue
		}
		if err := r.AnswerAccusation(ctx, t.match, outpoint); err != nil {
			r.log.Warnf("table %s: answering the claim at %s: %v", t.match, outpoint, err)
		}
	}
}
