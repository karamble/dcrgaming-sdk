// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package bridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	gamingwire "github.com/karamble/dcrgaming-sdk/pkg/gaming/wire"
)

// The conformance tests run a real bridge and its listener on a fake host and
// drive them with the client every game links. Calls cross real mTLS on a
// loopback port, and money reaches the host's wallet and node only through
// Host.

const (
	confGame     = "confgame"
	confStake    = 20_000_000
	confRefund   = 288
	confBond     = 1_000_000
	confBondLock = 2016
)

func confPolicy() GamePolicy {
	return GamePolicy{Account: "gaming", PerTableCapAtoms: 50_000_000, PerDayCapAtoms: 100_000_000, ApprovalTimeoutSecs: 120}
}

// confSeat is one wallet app running a bridge, with a game connected to it.
type confSeat struct {
	host   *fakeHost
	br     *Bridge
	game   *transport.Bridge
	frames <-chan transport.InboundFrame
}

func newConfSeat(t *testing.T, ctx context.Context, chain *fakeChain, name string, policy GamePolicy) *confSeat {
	t.Helper()
	h, br, cfg := startConfBridge(t, chain, name, policy)
	connect.Stamp(&cfg, connect.Identity{GameID: confGame, GameVer: 1, ClientVersion: "conformance"})
	game, err := transport.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = game.Close() })
	confEventually(t, "the game to say hello", func() bool {
		_, err := game.Hello(ctx, fakeParams.Name)
		return err == nil
	})
	frames, err := game.Events(ctx)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go answerRequests(ctx, game)
	confEventually(t, "the game to subscribe", func() bool { return br.gamingGameConnected(confGame) })
	return &confSeat{host: h, br: br, game: game, frames: frames}
}

// startConfBridge brings up one wallet app's bridge with the game registered,
// and returns the connection its operator would give the game.
func startConfBridge(t *testing.T, chain *fakeChain, name string, policy GamePolicy) (*fakeHost, *Bridge, transport.BridgeConfig) {
	t.Helper()
	h := newFakeHost(chain, name)
	for range 4 {
		if _, err := h.wallet.fund(fakeGamingAccount, 50_000_000); err != nil {
			t.Fatal(err)
		}
	}
	br := New(t.TempDir(), h.host())
	// No replay unasked: a frame that waits is applied by what it waited for.
	br.financialReplayEvery = time.Hour
	settings := GamingSettings{Enabled: true, RegisteredGames: []string{confGame}, Policies: map[string]GamePolicy{confGame: policy}}
	if _, err := br.WriteGamingSettings(settings, true, true); err != nil {
		t.Fatalf("register the game: %v", err)
	}
	cred, err := br.IssueGamingCredential(confGame)
	if err != nil {
		t.Fatalf("issue the game's credential: %v", err)
	}
	if err := br.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(br.Stop)
	confEventually(t, "the bridge to listen", func() bool { return br.server.Addr() != "" })
	return h, br, transport.BridgeConfig{
		Addr:       br.server.Addr(),
		ClientCert: []byte(cred.CertPEM),
		ClientKey:  []byte(cred.KeyPEM),
		BridgeCert: []byte(cred.BridgeCertPEM),
	}
}

// answerRequests does what a game does with what the operator asks of it:
// takes every invitation, and reports an empty state.
func answerRequests(ctx context.Context, game *transport.Bridge) {
	for {
		var req *gamingpb.BridgeRequest
		select {
		case <-ctx.Done():
			return
		case req = <-game.Requests():
		}
		reply := &gamingpb.RespondRequest{RequestId: req.GetRequestId(), Ok: true}
		switch {
		case req.GetAcceptInvite() != nil:
			u, err := url.Parse(req.GetAcceptInvite().GetInvite())
			if err != nil {
				reply.Ok, reply.Error = false, err.Error()
				break
			}
			reply.Result = &gamingpb.RespondRequest_AcceptInvite{AcceptInvite: &gamingpb.AcceptInviteResult{Sid: u.Query().Get("sid")}}
		case req.GetRefreshState() != nil:
			reply.Result = &gamingpb.RespondRequest_State{State: &gamingpb.GameState{}}
		}
		_ = game.Respond(ctx, reply)
	}
}

// accept has the operator accept an invitation to a two-seat table in gcid.
func (s *confSeat) accept(t *testing.T, ctx context.Context, sid, gcid string) {
	t.Helper()
	q := url.Values{}
	for k, v := range map[string]string{
		"fv": "2", "sid": sid, "seats": "2",
		"buyin": strconv.Itoa(confStake), "csv": strconv.Itoa(confRefund),
		"until": strconv.FormatInt(s.host.chain.height()+100, 10),
		"bond":  strconv.Itoa(confBond), "bondcsv": strconv.Itoa(confBondLock),
		"tablebond": "0", "tablebondcsv": "0",
	} {
		q.Set(k, v)
	}
	got, err := s.br.AcceptGamingInvite(ctx, confGame, "gaming://"+confGame+"/table?"+q.Encode(), gcid)
	if err != nil || got != sid {
		t.Fatalf("accept the table = %q, %v", got, err)
	}
}

// requestBond prepares a table's seat bond and asks for it to be paid.
func (s *confSeat) requestBond(t *testing.T, ctx context.Context, sid string) (*gamingpb.PreparedDeposit, transport.Spend) {
	t.Helper()
	if _, err := s.game.FinancialKey(ctx, sid); err != nil {
		t.Fatalf("financial key: %v", err)
	}
	dep, err := s.game.PrepareDeposit(ctx, &gamingpb.PrepareDepositRequest{
		Sid: sid, Kind: "seatbond", AmountAtoms: confBond, LockBlocks: confBondLock, IdentityKey: confKey(t),
	})
	if err != nil {
		t.Fatalf("prepare the seat bond: %v", err)
	}
	spend, err := s.game.RequestDepositSpend(ctx, dep.GetId(), dep.GetAddress(), confBond, "seat bond")
	if err != nil {
		t.Fatalf("ask to pay the seat bond: %v", err)
	}
	return dep, spend
}

func confKey(t *testing.T) string {
	t.Helper()
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(priv.PubKey().SerializeCompressed())
}

func confGCID(b byte) (string, [32]byte) {
	var id [32]byte
	for i := range id {
		id[i] = b
	}
	return hex.EncodeToString(id[:]), id
}

func confFrame(t *testing.T, sid, payload string) string {
	t.Helper()
	frames, err := gamingwire.Encode(confGame, 1, sid, []byte(payload), time.Time{}, 0)
	if err != nil || len(frames) != 1 {
		t.Fatalf("encode a frame: %v", err)
	}
	return frames[0]
}

func confContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func confEventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A game's credential introduces it as the game it was issued to, on the
// bridge's chain, under the operator's policy.
func TestConformanceAGameMeetsItsBridge(t *testing.T) {
	ctx := confContext(t)
	s := newConfSeat(t, ctx, newFakeChain(1000), "alice", confPolicy())
	reply, err := s.game.Hello(ctx, fakeParams.Name)
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetGame() != confGame || reply.GetNetwork() != fakeParams.Name ||
		!reply.GetPolicy().GetAccountBound() || reply.GetPolicy().GetPerTableCapAtoms() != confPolicy().PerTableCapAtoms {
		t.Fatalf("hello = %+v", reply)
	}
}

// The bridge serves no game while its operator is unprotected, and serves
// again once they are.
func TestConformanceAnUnprotectedOperatorClosesThePort(t *testing.T) {
	ctx := confContext(t)
	s := newConfSeat(t, ctx, newFakeChain(1000), "alice", confPolicy())
	hello := func() error {
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := s.game.Hello(call, fakeParams.Name)
		return err
	}
	s.host.operator.unprotected.Store(true)
	confEventually(t, "the port to close", func() bool { return hello() != nil })
	s.host.operator.unprotected.Store(false)
	confEventually(t, "the port to open again", func() bool { return hello() == nil })
}

// Frames of an accepted table cross whole, in both directions; a group the
// operator never accepted is carried neither way.
func TestConformanceFramesCrossTheBridge(t *testing.T) {
	ctx := confContext(t)
	s := newConfSeat(t, ctx, newFakeChain(1000), "alice", confPolicy())
	gcid, gcidRaw := confGCID(0xab)
	stray, strayRaw := confGCID(0xcd)
	_, peer := confGCID(0x22)
	const sid = "0123456789abcdef"
	s.accept(t, ctx, sid, gcid)

	in := confFrame(t, sid, "their move")
	if err := s.br.ReceiveGroupMessage(GroupMessage{GCID: gcidRaw, From: peer, Text: in, Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-s.frames:
		if got.Frame != in || got.GCID != gcid || got.From != hex.EncodeToString(peer[:]) {
			t.Fatalf("delivered %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the frame never reached the game")
	}

	if err := s.br.ReceiveGroupMessage(GroupMessage{GCID: strayRaw, From: peer, Text: confFrame(t, sid, "stray"), Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-s.frames:
		t.Fatalf("a frame from a group nobody accepted reached the game: %+v", got)
	case <-time.After(500 * time.Millisecond):
	}

	out := confFrame(t, sid, "my move")
	if err := s.game.SendGC(ctx, gcid, out); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !slices.Contains(s.host.relay.sentTo(gcidRaw), out) {
		t.Fatal("the game's frame did not leave through the host's Bison Relay client")
	}
	if err := s.game.SendGC(ctx, stray, confFrame(t, sid, "elsewhere")); err == nil || len(s.host.relay.sentTo(strayRaw)) != 0 {
		t.Fatalf("a frame went to a group nobody accepted: %v", err)
	}
}

// What a game reads of the chain is what the host's node says.
func TestConformanceTheChainIsTheHostsNode(t *testing.T) {
	ctx := confContext(t)
	chain := newFakeChain(1000)
	s := newConfSeat(t, ctx, chain, "alice", confPolicy())
	coin, err := s.host.wallet.fund(0, 12_345_678)
	if err != nil {
		t.Fatal(err)
	}
	chain.mine(1)

	tip, err := s.game.ChainTip(ctx)
	best, height, _ := chain.GetBestBlock(ctx)
	if err != nil || tip.Height != height || tip.Hash != best.String() {
		t.Fatalf("tip = %+v, %v", tip, err)
	}
	hash, err := s.game.BlockHash(ctx, 500)
	want, _ := chain.GetBlockHash(ctx, 500)
	if err != nil || hash != want.String() {
		t.Fatalf("block 500 = %s, %v", hash, err)
	}
	out, err := s.game.Outpoint(ctx, coin.Hash.String(), coin.Index)
	if err != nil || !out.Found || out.ValueAtoms != 12_345_678 || out.Confirmations != 2 {
		t.Fatalf("outpoint = %+v, %v", out, err)
	}
}

// A payment a game asks for waits for the operator: the host's wallet builds
// it, and signs and publishes it only on the operator's approval with the
// right passphrase. The seat bond's approval also proves the table's key.
func TestConformanceAPaymentWaitsForTheOperator(t *testing.T) {
	ctx := confContext(t)
	chain := newFakeChain(1000)
	s := newConfSeat(t, ctx, chain, "alice", confPolicy())
	gcid, gcidRaw := confGCID(0xab)
	const sid = "0123456789abcdef"
	s.accept(t, ctx, sid, gcid)
	dep, spend := s.requestBond(t, ctx, sid)
	w := s.host.wallet

	if !slices.Contains(w.imported(), dep.GetRedeemScript()) {
		t.Fatal("the host's wallet was not told to watch the escrow")
	}
	if spend.State != transport.SpendPending || w.called("Construct") != 1 || w.called("SignTransaction") != 0 || w.called("Publish") != 0 {
		t.Fatalf("before approval: %+v, calls %v", spend, w.callCounts())
	}

	if _, err := s.br.ApproveGamingSpend(ctx, spend.ID, []byte("wrong")); err == nil {
		t.Fatal("a wrong passphrase approved a payment")
	}
	if w.called("Publish") != 0 || chain.mempoolSize() != 0 {
		t.Fatal("a refused approval moved coin")
	}

	approved, err := s.br.ApproveGamingSpend(ctx, spend.ID, []byte(w.pass))
	if err != nil || approved.State != GamingSpendApproved || approved.TxID == "" {
		t.Fatalf("approve = %+v, %v", approved, err)
	}
	hash, err := chainhash.NewHashFromStr(approved.TxID)
	if err != nil {
		t.Fatal(err)
	}
	paid, err := chain.GetRawTransaction(ctx, hash)
	if err != nil {
		t.Fatalf("the payment is not on the chain: %v", err)
	}
	pk, _ := hex.DecodeString(dep.GetPkScript())
	if !slices.ContainsFunc(paid.MsgTx().TxOut, func(o *wire.TxOut) bool {
		return o.Value == confBond && bytes.Equal(o.PkScript, pk)
	}) {
		t.Fatal("the published payment does not pay the escrow the bond")
	}
	if w.called("SignHash") == 0 || !slices.ContainsFunc(s.host.relay.sentTo(gcidRaw), func(text string) bool {
		return strings.Contains(text, "authority=")
	}) {
		t.Fatal("the seat bond's approval did not prove and announce the table's key")
	}
	status, err := s.game.SpendStatus(ctx, spend.ID)
	if err != nil || status.State != transport.SpendApproved || status.TxID != approved.TxID {
		t.Fatalf("the game sees %+v, %v", status, err)
	}
}

// A payment the operator denies never reaches the wallet's keys.
func TestConformanceADeniedPaymentMovesNothing(t *testing.T) {
	ctx := confContext(t)
	s := newConfSeat(t, ctx, newFakeChain(1000), "alice", confPolicy())
	gcid, _ := confGCID(0xab)
	const sid = "0123456789abcdef"
	s.accept(t, ctx, sid, gcid)
	_, spend := s.requestBond(t, ctx, sid)

	if denied, err := s.br.DenyGamingSpend(spend.ID); err != nil || denied.State != GamingSpendDenied {
		t.Fatalf("deny = %+v, %v", denied, err)
	}
	if _, err := s.br.ApproveGamingSpend(ctx, spend.ID, []byte(s.host.wallet.pass)); err == nil {
		t.Fatal("a denied payment was approved afterwards")
	}
	status, err := s.game.SpendStatus(ctx, spend.ID)
	if err != nil || status.State != transport.SpendDenied {
		t.Fatalf("the game sees %+v, %v", status, err)
	}
	w := s.host.wallet
	if w.called("SignTransaction") != 0 || w.called("WithUnlockedAccount") != 0 || w.called("Publish") != 0 {
		t.Fatalf("a denied payment reached the wallet's keys: %v", w.callCounts())
	}
}

// A deposit over the operator's table cap is refused before anything is
// registered or built.
func TestConformanceACapRefusesADeposit(t *testing.T) {
	ctx := confContext(t)
	policy := confPolicy()
	policy.PerTableCapAtoms = confBond - 1
	s := newConfSeat(t, ctx, newFakeChain(1000), "alice", policy)
	gcid, _ := confGCID(0xab)
	const sid = "0123456789abcdef"
	s.accept(t, ctx, sid, gcid)
	if _, err := s.game.FinancialKey(ctx, sid); err != nil {
		t.Fatal(err)
	}
	_, err := s.game.PrepareDeposit(ctx, &gamingpb.PrepareDepositRequest{
		Sid: sid, Kind: "seatbond", AmountAtoms: confBond, LockBlocks: confBondLock, IdentityKey: confKey(t),
	})
	if err == nil {
		t.Fatal("a deposit over the table cap was prepared")
	}
	if len(s.host.wallet.imported()) != 0 || s.host.wallet.called("Construct") != 0 {
		t.Fatal("a refused deposit reached the wallet")
	}
}
