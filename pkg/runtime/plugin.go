package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/evidence"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/punish"
)

// gcID is the group chat a table lives in: 64 lowercase hex characters.
var gcID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// serveRequests answers the bridge's control requests until the context ends.
//
// This is the plug-in point, and it is the reason a game no longer writes a
// dispatcher: there are exactly five requests, both existing games hand-wrote
// the same five-case switch, and getting one of them subtly wrong is a table
// the dashboard can no longer manage.
func (r *Runtime) serveRequests(ctx context.Context) error {
	reqs := r.bridge.Requests()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req, ok := <-reqs:
			if !ok {
				return nil
			}
			r.answer(ctx, req)
		}
	}
}

// answer carries out one request and replies exactly once.
//
// A reply is always sent, including for a request that failed and for one this
// runtime does not recognise. A bridge left waiting on a request nobody
// answered is a dashboard stuck on a spinner, which an operator reads as the
// game being broken.
func (r *Runtime) answer(ctx context.Context, req *gamingpb.BridgeRequest) {
	reply := &gamingpb.RespondRequest{RequestId: req.GetRequestId()}

	work, cancel := ctxFor(ctx, req)
	defer cancel()

	if err := r.doRequest(work, req, reply); err != nil {
		reply.Ok, reply.Error = false, err.Error()
		r.log.Warnf("%s: %v", describe(req), err)
	} else {
		reply.Ok = true
	}

	// The answer gets its own budget, taken from the parent rather than
	// from the work. A request that ran past the console's deadline is
	// exactly the one whose answer is worth delivering, and sending it on
	// the expired context would fail before it left.
	say, done := context.WithTimeout(ctx, answerBudget)
	defer done()
	if err := r.bridge.Respond(say, reply); err != nil {
		r.log.Warnf("could not answer %s: %v", describe(req), err)
	}
}

// answerBudget is how long delivering an answer may take. Not retried: the
// console polls state for anything that matters, and a retry loop against a
// bridge that has gone away is this process stuck.
const answerBudget = 30 * time.Second

// ctxFor bounds a request by the deadline the console set, except for a
// reclaim.
//
// A reclaim signs and broadcasts real coin. Abandoning one half way because
// somebody closed a tab would leave money moving with nobody watching it, so it
// runs to completion or fails on its own terms. The other four read state or
// write down a preference, and none of them is worse for being cut short.
func ctxFor(ctx context.Context, req *gamingpb.BridgeRequest) (context.Context, context.CancelFunc) {
	if _, isReclaim := req.GetReq().(*gamingpb.BridgeRequest_Reclaim); isReclaim {
		return ctx, func() {}
	}
	if d := req.GetDeadlineUnix(); d > 0 {
		return context.WithDeadline(ctx, time.Unix(d, 0))
	}
	return ctx, func() {}
}

// doRequest is the five-case dispatch. Nothing here decides anything about
// money; each case hands off to the stage that owns it.
func (r *Runtime) doRequest(ctx context.Context, req *gamingpb.BridgeRequest, reply *gamingpb.RespondRequest) error {
	switch body := req.GetReq().(type) {
	case *gamingpb.BridgeRequest_AcceptInvite:
		match, err := r.acceptInvite(ctx, body.AcceptInvite)
		if err != nil {
			return err
		}
		reply.Result = &gamingpb.RespondRequest_AcceptInvite{
			AcceptInvite: &gamingpb.AcceptInviteResult{Sid: match},
		}
		return nil

	case *gamingpb.BridgeRequest_Reclaim:
		txid, err := r.doReclaim(ctx, body.Reclaim)
		if err != nil {
			return err
		}
		reply.Result = &gamingpb.RespondRequest_Reclaim{
			Reclaim: &gamingpb.ReclaimResult{Txid: txid},
		}
		return nil

	case *gamingpb.BridgeRequest_SetPayout:
		return r.setPayout(ctx, body.SetPayout.GetAddress())

	case *gamingpb.BridgeRequest_SetNames:
		r.setNames(body.SetNames.GetNames())
		return nil

	case *gamingpb.BridgeRequest_RefreshState:
		st := r.gameState(ctx)
		// Set only when the state answers a RefreshState, which is how
		// the console tells this from an unsolicited report.
		st.RequestId = req.GetRequestId()
		reply.Result = &gamingpb.RespondRequest_State{State: st}
		return nil
	}
	return fmt.Errorf("this game does not know how to answer that request")
}

// describe names a request for a log line, without repeating its contents.
func describe(req *gamingpb.BridgeRequest) string {
	switch req.GetReq().(type) {
	case *gamingpb.BridgeRequest_AcceptInvite:
		return "accepting an invite"
	case *gamingpb.BridgeRequest_Reclaim:
		return "reclaiming locked money"
	case *gamingpb.BridgeRequest_SetPayout:
		return "setting the payout address"
	case *gamingpb.BridgeRequest_SetNames:
		return "setting the table's names"
	case *gamingpb.BridgeRequest_RefreshState:
		return "reporting state"
	}
	return "an unknown request"
}

// setPayout records where winnings should go.
func (r *Runtime) setPayout(_ context.Context, addr string) error {
	if strings.TrimSpace(addr) == "" {
		return fmt.Errorf("a payout address is required")
	}
	r.mu.Lock()
	r.payout = addr
	r.mu.Unlock()
	return nil
}

// setNames records the operator's names for the identities at the table. They
// are decoration: nothing that moves money reads a name.
//
// Merged rather than replaced, and an empty name removes one, so a console can
// correct a single entry without restating the rest.
func (r *Runtime) setNames(names map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names == nil {
		r.names = map[string]string{}
	}
	for k, v := range names {
		if strings.TrimSpace(v) == "" {
			delete(r.names, k)
			continue
		}
		r.names[k] = v
	}
}

// gameState asks the game what to show, and adds what the runtime knows.
//
// The game's answer is never allowed to stop a report: an operator looking at a
// stuck game needs the dashboard most, so a panic in the game's own State is
// caught and reported rather than left to take the request down.
func (r *Runtime) gameState(ctx context.Context) (st *gamingpb.GameState) {
	st = &gamingpb.GameState{PayoutAddress: r.Payout()}
	defer func() {
		if p := recover(); p != nil {
			st.ChainErr = fmt.Sprintf("the game could not report its state: %v", p)
			r.log.Errorf("the game panicked reporting state: %v", p)
		}
	}()

	gs := r.rules.State(ctx)
	for _, t := range gs.Tables {
		st.Tables = append(st.Tables, &gamingpb.Table{
			Sid: t.Match, State: t.Status, Seats: t.Seats,
		})
	}
	return st
}

// Seize takes a seat's forfeitable bond, using the key its own signatures gave
// up.
//
// The bond is spent with two halves of one key: the half the equivocation
// published, and the half only this seat holds. Neither alone opens it, which
// is what makes the punishment self-executing rather than something anybody has
// to be trusted to carry out.
//
// What authorises this is not the game's word and not the shape of any proof.
// It is escrow arithmetic, and it holds because of three things this runtime
// guarantees and escrow.ForfeitIndex explicitly does not:
//
//   - the bond script is derived here from the roster, never accepted from the
//     wire (buildForfeitableBonds, and re-derived on store load);
//   - the punishment key is this seat's own, from its seed;
//   - the branch names this seat's own session key.
//
// Given those, a key that is not the accused's opens no branch: escrow.ForfeitIndex
// refuses it offline before anything is built, and the script engine in
// escrow's finishBondSpend refuses the assembled spend before the bytes leave
// this process. So the name is literal - this spends a branch whose secret the
// accused exposed. It is not a general power to punish a cheat: a game whose
// cheating exposes no key has nothing for this verb to spend, and the ladder is
// its only lever.
//
// The key is used and not kept: it is never stored on the table and never
// logged.
func (r *Runtime) Seize(ctx context.Context, match string, seat uint32, exposed *evidence.Exposed) error {
	recovered := exposed.Key()
	if recovered == nil {
		return fmt.Errorf("no key was exposed, so there is no bond to seize")
	}
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	mine, ok := t.form.OurSeat()
	if !ok {
		return fmt.Errorf("this table has not seated us")
	}
	if seat == mine {
		// Reachable, and worth its own sentence: without it this fails
		// four frames down as "this bond has no punishment branch for
		// that key", which reads like a derivation bug.
		return fmt.Errorf("seat %d is this seat, and a bond is not seized from oneself", seat)
	}
	bond, ok := r.ForfeitableBond(match, seat)
	if !ok {
		return fmt.Errorf("seat %d has no forfeitable bond; every seat has to announce first", seat)
	}
	r.mu.Lock()
	funded, isFunded := t.forfeitFunded[seat]
	punisher := t.punish
	r.mu.Unlock()
	if !isFunded || funded.outpoint == "" {
		return fmt.Errorf("seat %d's forfeitable bond is not on the chain, so there is nothing to take", seat)
	}
	if punisher == nil {
		return fmt.Errorf("this seat holds no punishment key for that table")
	}
	if r.isSweeping(funded.outpoint) {
		return fmt.Errorf("a spend of %s is already on its way from this process; "+
			"asking again would be a double spend", funded.outpoint)
	}

	seats, _ := t.form.Seats()
	matchID, ok := t.form.RosterHash()
	if !ok {
		return fmt.Errorf("this table has no settled roster")
	}
	script, err := hex.DecodeString(bond.ScriptHex)
	if err != nil || len(script) == 0 {
		return fmt.Errorf("seat %d's forfeitable bond has no script", seat)
	}
	prevout, err := outpointOf(funded.outpoint)
	if err != nil {
		return err
	}
	pinned, err := r.pinnedPayout()
	if err != nil {
		return err
	}

	tx, err := punish.SweepForfeited(recovered, pinned, punish.Sweep{
		Bond:       script,
		Prevout:    prevout,
		ValueAtoms: funded.atoms,
		FeeAtoms:   r.reclaimFee,
		Branch:     forfeit.Branch{Match: hex.EncodeToString(matchID[:]), Seat: seats[mine]},
		Punisher:   punisher,
		Params:     r.params,
	})
	if err != nil {
		return fmt.Errorf("build the sweep: %w", err)
	}
	raw, err := tx.Bytes()
	if err != nil {
		return err
	}
	txid, err := r.bridge.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		return fmt.Errorf("send the sweep: %w", err)
	}
	r.noteSweeping(funded.outpoint)
	r.log.Infof("table %s: took seat %d's forfeitable bond in %s", match, seat, txid)
	return nil
}

// pinnedPayout is where a punishment spend has to pay, and the only place it
// may. Without one there is nothing holding the spend to this operator.
func (r *Runtime) pinnedPayout() ([]byte, error) {
	addr := r.Payout()
	if addr == "" {
		return nil, fmt.Errorf("no payout address has been set, so a punishment spend has nowhere it is allowed to go")
	}
	return payScriptFor(addr, r.params)
}
