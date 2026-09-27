// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package bridge

import (
	"context"
	"sync/atomic"
	"testing"

	chainjson "github.com/decred/dcrd/rpc/jsonrpc/types/v4"
)

// fakeDcrd answers getinfo with whatever the caller currently wants, so a test
// can change the node's mind between calls the way restarting dcrd does.
type fakeDcrd struct {
	Chain
	txIndex *atomic.Bool
}

func (d fakeDcrd) GetInfo(context.Context) (*chainjson.InfoChainResult, error) {
	return &chainjson.InfoChainResult{TxIndex: d.txIndex.Load()}, nil
}

// The operator's fix is to set txindex=1 and restart dcrd, and dcrpulse keeps
// running across that. A cached answer would go on refusing after the work was
// already done, which is a worse failure than the one the gate prevents - and
// an invisible one, since nothing about a stale "no" looks wrong.
func TestTheIndexIsReadFromTheNodeEveryTime(t *testing.T) {
	br := newTestBridge(t)
	var txIndex atomic.Bool
	br.host.Node = func() Chain { return fakeDcrd{txIndex: &txIndex} }
	ctx := context.Background()

	if br.TxIndexActive(ctx) {
		t.Fatal("a node with no index read as having one")
	}

	// dcrd restarted with txindex=1. dcrpulse did not.
	txIndex.Store(true)
	if !br.TxIndexActive(ctx) {
		t.Fatal("the index stayed cached off after the node was fixed, so the bridge would never switch on")
	}

	// And back, so an index that goes away is noticed too.
	txIndex.Store(false)
	if br.TxIndexActive(ctx) {
		t.Fatal("the index stayed cached on after the node lost it")
	}
}

// A dcrd that cannot be asked is not a dcrd that answered yes. Refusing is
// recoverable and says what to do; enabling on a guess parks the first payout
// at publishing with nothing to read.
func TestAnUnreachableNodeReadsAsNoIndex(t *testing.T) {
	br := newTestBridge(t)

	if _, err := br.DcrdHasTxIndex(context.Background()); err == nil {
		t.Fatal("an absent dcrd answered without an error")
	}
	if br.TxIndexActive(context.Background()) {
		t.Fatal("an absent dcrd was taken as having the index")
	}
}
