package runtime

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/ruling"
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

	if d := req.GetDeadlineUnix(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Unix(d, 0))
		defer cancel()
	}

	if err := r.doRequest(ctx, req, reply); err != nil {
		reply.Ok, reply.Error = false, err.Error()
		r.log.Warnf("%s: %v", describe(req), err)
	} else {
		reply.Ok = true
	}
	if err := r.bridge.Respond(ctx, reply); err != nil {
		r.log.Warnf("could not answer %s: %v", describe(req), err)
	}
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
		reply.Result = &gamingpb.RespondRequest_State{State: r.gameState(ctx)}
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

// Forfeit carries out a ruling the game has made.
//
// An equivocation ruling is checked here before anything is spent - the key
// either falls out of the two signatures or it does not, so a game cannot cause
// a bond to be swept by asserting that someone cheated. A silence ruling is
// taken at the game's word, because the ladder gives the accused an on-chain
// right of reply and the SDK has no vocabulary for what was owed.
//
// The mechanism itself is pkg/punish and pkg/evidence. What is still missing is
// the table's forfeitable-bond records, which arrive with the funding stage.
func (r *Runtime) Forfeit(_ context.Context, rl ruling.Ruling) error {
	if err := rl.Validate(); err != nil {
		return err
	}
	if rl.Kind == ruling.Equivocation {
		if _, err := rl.Equivocated.Recover(); err != nil {
			return fmt.Errorf("this equivocation exposes no key, so no bond may be swept: %w", err)
		}
	}
	return fmt.Errorf("forfeiting: %w", ErrNotYet)
}
