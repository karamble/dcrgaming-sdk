// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package bridge

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridge/funds"
)

// The announcement that completes a table's roster lets the commitments that
// arrived before it apply, so the bridge asks for them to be tried again at
// once rather than at its next replay.
func TestACompletedRosterRetriesWhatWaitedForIt(t *testing.T) {
	ctx := context.Background()
	br := New(t.TempDir(), newFakeHost(newFakeChain(10), "alice").host())
	store, _ := payoutLedger(t, br, "awaiting_signatures")
	scope := funds.Scope{Game: "poker", Network: "mainnet", Wallet: "fp"}
	br.receiveScope = func(context.Context, string) (funds.Scope, error) { return scope, nil }
	br.receiveParams = func(context.Context) (*chaincfg.Params, error) { return chaincfg.MainNetParams(), nil }
	accepted, err := store.AuthorizedTable(scope, proofSID)
	if err != nil {
		t.Fatal(err)
	}
	own, err := store.WalletKey(scope, proofSID)
	if err != nil {
		t.Fatal(err)
	}
	proof, key := signedProof(t, 5, prunePeer, proofSID, pruneGCA, accepted.TermsHash())
	if err := store.BindSeats(scope, proofSID, []string{hex.EncodeToString(key), own.Public}); err != nil {
		t.Fatal(err)
	}
	if err := br.announceGamingAuthority(ctx, scope, proofSID); err != nil {
		t.Fatal(err)
	}
	if len(br.financialRetry) != 0 {
		t.Fatal("a roster still a seat short asked for a retry")
	}

	_, frame, err := financialFrame("poker", proofSID, financialMessage{Key: hex.EncodeToString(key), Proof: proof})
	if err != nil {
		t.Fatal(err)
	}
	if err := br.receiveFinancialFrame(ctx, GamingFrameEvent{Game: "poker", GCID: pruneGCA, From: prunePeer, Frame: frame, Financial: true}); err != nil {
		t.Fatal(err)
	}
	if len(br.financialRetry) != 1 {
		t.Fatal("completing the roster did not retry what waited for it")
	}
}
