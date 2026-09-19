package runtime

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

// gcID is the group chat a table lives in: 64 lowercase hex characters.
var gcID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// serveRequests answers the bridge's control requests until the context ends.
//
// This is the plug-in point, and it is the reason a game no longer writes a
// dispatcher: getting one of these cases subtly wrong is a table
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

// ctxFor bounds a request by the deadline the console set.
func ctxFor(ctx context.Context, req *gamingpb.BridgeRequest) (context.Context, context.CancelFunc) {
	if d := req.GetDeadlineUnix(); d > 0 {
		return context.WithDeadline(ctx, time.Unix(d, 0))
	}
	return ctx, func() {}
}

// doRequest handles nonfinancial operator requests. Money never enters this
// game-control channel.
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
	case *gamingpb.BridgeRequest_SetNames:
		return "setting the table's names"
	case *gamingpb.BridgeRequest_RefreshState:
		return "reporting state"
	}
	return "an unknown request"
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
	st = &gamingpb.GameState{}
	defer func() {
		if p := recover(); p != nil {
			st.ChainErr = fmt.Sprintf("the game could not report its state: %v", p)
			r.log.Errorf("the game panicked reporting state: %v", p)
		}
	}()

	// The runtime knows the table, the chat, the buy-in, the deadline and
	// whether it is over, so it fills those itself. A game only decides the
	// status line, and only if it wants to.
	r.mu.Lock()
	matches := make([]string, 0, len(r.tables))
	for match := range r.tables {
		matches = append(matches, match)
	}
	r.mu.Unlock()
	sort.Strings(matches)

	rows := make(map[string]*gamingpb.Table, len(matches))
	for _, match := range matches {
		snap, err := r.Snapshot(match)
		if err != nil {
			continue
		}
		row := &gamingpb.Table{
			Sid:        match,
			Gcid:       snap.Record.GCID,
			State:      snap.Phase,
			Seats:      snap.Record.Terms.Seats,
			BuyinAtoms: int64(snap.Record.Terms.BuyInAtoms),
			Until:      snap.Record.Terms.Until,
			Over:       snap.Record.RecoveryOnly || snap.Record.Aborted,
		}
		rows[match] = row
		st.Tables = append(st.Tables, row)
	}

	if hook, ok := r.rules.(Reporting); ok {
		for _, t := range hook.State(ctx).Tables {
			if row, ok := rows[t.Match]; ok && t.Status != "" {
				row.State = t.Status
			}
		}
	}
	return st
}
