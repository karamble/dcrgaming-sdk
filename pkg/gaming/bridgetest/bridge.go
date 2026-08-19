// Package bridgetest is a dcrpulse gaming bridge that only exists in your
// process, so a game can test the way it handles money without a wallet, a
// node, a bridge or a person to approve anything.
//
// It exists because both games that reached mainnet built one of these
// privately first - dcrpoker its harness, dcrbattleships its relay - and a
// third game would have built a third. A game cannot test its money handling
// without one, so shipping it is part of shipping the runtime.
//
// # Why the failures matter more than the happy path
//
// A fake that always answers is the easy half and the less useful one. The
// failures a game must survive are the reason this is here:
//
//   - Unreachable. Every call answers codes.Unavailable, which is what
//     transport.Unreachable detects. This is the one that costs coin when a game
//     gets it wrong: not being able to ask whether a payment happened is not the
//     same as being told it did not, and a game that treats them alike pays
//     twice. Turn it on mid-flight and a correct game keeps asking.
//   - Held spends. A request that no person has answered yet stays pending
//     forever, which is the normal state of a real bridge waiting on a human.
//   - Refused spends. The bridge answered, and the answer was no. Terminal, and
//     the opposite of unreachable in every way that matters.
//
// # What it does not do
//
// It does not validate scripts, check signatures, enforce spend caps or ask
// anyone for a passphrase. A real bridge is the policy boundary; this is a
// stand-in for its shape, not for its judgement. A transaction handed to
// Broadcast is taken at its word and its outputs appear on the fake chain.
package bridgetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
)

// Verdict is what the fake bridge does with a spend request.
type Verdict uint8

const (
	// Approve pays it, and the output appears on the fake chain one block
	// later. The default, because most tests are not about refusal.
	Approve Verdict = iota
	// Refuse answers no. Terminal: the money did not move and never will.
	Refuse
	// Hold answers nothing. The request stays pending, which is what a real
	// bridge does while it waits for a person.
	Hold
)

// Utxo is one output on the fake chain.
type Utxo struct {
	PkScript  []byte
	Value     int64
	ConfirmAt int64
}

// Options configures a fake bridge.
type Options struct {
	// Game and Network are what Hello answers with. A game that checks the
	// bridge is on the chain it expected will compare against these.
	Game    string
	Network string
	// Params decodes the addresses spend requests name.
	Params stdaddr.AddressParams
	// Height is the fake chain's starting tip.
	Height int64
}

// Bridge is an in-process stand-in for a dcrpulse gaming bridge.
//
// Safe for concurrent use: a game under test is talking to it from several
// goroutines, which is the situation the real thing is in too.
type Bridge struct {
	gamingpb.UnimplementedBridgeServiceServer

	opts Options

	mu      sync.Mutex
	height  int64
	nextID  int
	spends  map[string]*gamingpb.Spend
	utxos   map[string]Utxo
	sent    map[string]int
	rawtx   map[string][]byte
	subs    map[string]chan *gamingpb.Frame
	verdict Verdict
	refusal string
	states  []*gamingpb.GameState

	unreachable atomic.Bool
	pushed      atomic.Int64
}

// New returns a fake bridge that is not yet serving. Call Serve.
func New(opts Options) *Bridge {
	if opts.Game == "" {
		opts.Game = "testgame"
	}
	if opts.Network == "" {
		opts.Network = "mainnet"
	}
	return &Bridge{
		opts: opts, height: opts.Height,
		spends: map[string]*gamingpb.Spend{},
		utxos:  map[string]Utxo{},
		sent:   map[string]int{},
		rawtx:  map[string][]byte{},
		subs:   map[string]chan *gamingpb.Frame{},
	}
}

// SetUnreachable makes every call answer codes.Unavailable, or stops doing so.
//
// This is the failure a game must survive without losing money: it says only
// that this process could not ask, never that the answer was no. A spend held
// across an unreachable window is still a spend that may have been paid.
func (b *Bridge) SetUnreachable(on bool) { b.unreachable.Store(on) }

// SetVerdict decides what happens to spend requests from now on. The reason is
// used only by Refuse.
func (b *Bridge) SetVerdict(v Verdict, reason string) {
	b.mu.Lock()
	b.verdict, b.refusal = v, reason
	b.mu.Unlock()
}

// Settle answers a held spend after the fact, the way a person eventually
// would. Reports whether there was such a request to answer.
func (b *Bridge) Settle(id string, v Verdict, reason string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	sp, ok := b.spends[id]
	if !ok {
		return false
	}
	b.decide(sp, v, reason)
	return true
}

// Mine advances the fake chain.
func (b *Bridge) Mine(n int64) {
	b.mu.Lock()
	b.height += n
	b.mu.Unlock()
}

// SetHeight puts the fake chain at a height.
func (b *Bridge) SetHeight(h int64) {
	b.mu.Lock()
	b.height = h
	b.mu.Unlock()
}

// Height reports the fake chain tip.
func (b *Bridge) Height() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.height
}

// Place puts an output on the fake chain without anyone having spent to it,
// which is how a test arranges a bond that already exists.
func (b *Bridge) Place(txid string, vout uint32, pkScript []byte, value, confirmAt int64) {
	b.mu.Lock()
	b.utxos[outKey(txid, vout)] = Utxo{PkScript: pkScript, Value: value, ConfirmAt: confirmAt}
	b.mu.Unlock()
}

// Output reads an output straight off the fake chain.
func (b *Bridge) Output(txid string, vout uint32) (Utxo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.utxos[outKey(txid, vout)]
	return u, ok
}

// SentCount is how many times a transaction was broadcast. More than one is a
// game that rebroadcast, which is usually fine; more than one *payment* is not.
func (b *Bridge) SentCount(txid string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent[txid]
}

// Raw returns a broadcast transaction's bytes.
func (b *Bridge) Raw(txid string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, ok := b.rawtx[txid]
	return raw, ok
}

// Spends returns every spend request the bridge has seen, by id.
func (b *Bridge) Spends() map[string]*gamingpb.Spend {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]*gamingpb.Spend, len(b.spends))
	for k, v := range b.spends {
		out[k] = v
	}
	return out
}

// States returns every game state reported to the bridge.
func (b *Bridge) States() []*gamingpb.GameState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*gamingpb.GameState(nil), b.states...)
}

// Subscribers is how many seats are listening, which a test waits on before
// sending rather than sleeping and hoping.
func (b *Bridge) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Relayed is how many frames have been pushed to a subscriber, which a test
// waits on rather than sleeping.
func (b *Bridge) Relayed() int64 { return b.pushed.Load() }

func outKey(txid string, vout uint32) string { return fmt.Sprintf("%s:%d", txid, vout) }

// BlockHashHex is the fake chain's block hash at a height. Deterministic, so
// two seats reading the same height agree, which is what the real chain gives
// them.
func BlockHashHex(h uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], h)
	sum := blake256.Sum256(append([]byte("bridgetest/block/v1"), b[:]...))
	return hex.EncodeToString(sum[:])
}

// decide moves a spend to its answer. Caller holds the lock.
func (b *Bridge) decide(sp *gamingpb.Spend, v Verdict, reason string) {
	switch v {
	case Approve:
		addr, err := stdaddr.DecodeAddress(sp.GetAddress(), b.opts.Params)
		if err != nil {
			sp.State, sp.Error = string(transport.SpendFailed), err.Error()
			return
		}
		_, pkScript := addr.PaymentScript()
		sum := sha256.Sum256([]byte("bridgetest-pay-" + sp.GetId()))
		txid := hex.EncodeToString(sum[:])
		b.utxos[outKey(txid, 1)] = Utxo{
			PkScript: pkScript, Value: sp.GetAmountAtoms(), ConfirmAt: b.height + 1,
		}
		sp.State, sp.Txid = string(transport.SpendApproved), txid
	case Refuse:
		sp.State, sp.Error = string(transport.SpendDenied), reason
	case Hold:
		sp.State = string(transport.SpendPending)
	}
}

func (b *Bridge) Hello(_ context.Context, _ *gamingpb.HelloRequest) (*gamingpb.HelloReply, error) {
	return &gamingpb.HelloReply{Game: b.opts.Game, Network: b.opts.Network}, nil
}

// Subscribe registers the caller by its certificate name and delivers every
// frame relayed to it until the stream ends.
func (b *Bridge) Subscribe(_ *gamingpb.SubscribeRequest, stream grpc.ServerStreamingServer[gamingpb.BridgeEvent]) error {
	cn := callerCN(stream.Context())
	ch := make(chan *gamingpb.Frame, 4096)
	b.mu.Lock()
	b.subs[cn] = ch
	b.mu.Unlock()

	if err := stream.Send(&gamingpb.BridgeEvent{
		Event: &gamingpb.BridgeEvent_Start{Start: &gamingpb.StreamStart{Epoch: "e1"}},
	}); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case f := <-ch:
			if err := stream.Send(&gamingpb.BridgeEvent{
				Event: &gamingpb.BridgeEvent_Frame{Frame: f},
			}); err != nil {
				return err
			}
		}
	}
}

// SendFrame fans one seat's frame out to every other subscriber, never back to
// the sender.
func (b *Bridge) SendFrame(ctx context.Context, req *gamingpb.SendFrameRequest) (*gamingpb.SendFrameReply, error) {
	from := callerCN(ctx)
	frame := &gamingpb.Frame{Gcid: req.GetGcid(), From: from, Frame: req.GetFrame()}
	b.mu.Lock()
	for cn, ch := range b.subs {
		if cn == from {
			continue
		}
		ch <- frame
		b.pushed.Add(1)
	}
	b.mu.Unlock()
	return &gamingpb.SendFrameReply{}, nil
}

func (b *Bridge) ChainTip(_ context.Context, _ *gamingpb.ChainTipRequest) (*gamingpb.ChainTipReply, error) {
	b.mu.Lock()
	h := b.height
	b.mu.Unlock()
	return &gamingpb.ChainTipReply{Height: h, Hash: BlockHashHex(uint32(h))}, nil
}

func (b *Bridge) BlockHash(_ context.Context, req *gamingpb.BlockHashRequest) (*gamingpb.BlockHashReply, error) {
	return &gamingpb.BlockHashReply{Height: req.GetHeight(), Hash: BlockHashHex(req.GetHeight())}, nil
}

func (b *Bridge) Outpoint(_ context.Context, req *gamingpb.OutpointRequest) (*gamingpb.OutpointReply, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.utxos[outKey(req.GetTxid(), req.GetVout())]
	if !ok {
		return &gamingpb.OutpointReply{}, nil
	}
	if !req.GetIncludeMempool() && b.height < u.ConfirmAt {
		return &gamingpb.OutpointReply{}, nil
	}
	var conf int64
	if b.height >= u.ConfirmAt {
		conf = b.height - u.ConfirmAt + 1
	}
	return &gamingpb.OutpointReply{
		Found: true, ValueAtoms: u.Value,
		PkScriptHex: hex.EncodeToString(u.PkScript), Confirmations: conf,
	}, nil
}

func (b *Bridge) RequestSpend(_ context.Context, req *gamingpb.RequestSpendRequest) (*gamingpb.Spend, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	sp := &gamingpb.Spend{
		Id:          fmt.Sprintf("spend-%d", b.nextID),
		Address:     req.GetAddress(),
		AmountAtoms: req.GetAmountAtoms(),
		Reason:      req.GetReason(),
	}
	b.decide(sp, b.verdict, b.refusal)
	b.spends[sp.Id] = sp
	return sp, nil
}

func (b *Bridge) SpendStatus(_ context.Context, req *gamingpb.SpendStatusRequest) (*gamingpb.Spend, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sp, ok := b.spends[req.GetId()]
	if !ok {
		return &gamingpb.Spend{Id: req.GetId(), State: string(transport.SpendPending)}, nil
	}
	return sp, nil
}

func (b *Bridge) Broadcast(_ context.Context, req *gamingpb.BroadcastRequest) (*gamingpb.BroadcastReply, error) {
	raw, err := hex.DecodeString(req.GetRawTxHex())
	if err != nil {
		return nil, err
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	txid := tx.TxHash().String()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent[txid]++
	b.rawtx[txid] = raw
	for i, out := range tx.TxOut {
		b.utxos[outKey(txid, uint32(i))] = Utxo{
			PkScript: out.PkScript, Value: out.Value, ConfirmAt: b.height + 1,
		}
	}
	return &gamingpb.BroadcastReply{Txid: txid}, nil
}

func (b *Bridge) Respond(_ context.Context, _ *gamingpb.RespondRequest) (*gamingpb.RespondReply, error) {
	return &gamingpb.RespondReply{}, nil
}

func (b *Bridge) ReportState(_ context.Context, st *gamingpb.GameState) (*gamingpb.ReportStateReply, error) {
	b.mu.Lock()
	b.states = append(b.states, st)
	b.mu.Unlock()
	return &gamingpb.ReportStateReply{}, nil
}

// unreachableUnary and unreachableStream are how SetUnreachable reaches every
// method without each one testing a flag.
func (b *Bridge) unreachableUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if b.unreachable.Load() {
		return nil, status.Error(codes.Unavailable, "bridgetest: the bridge is unreachable")
	}
	return h(ctx, req)
}

func (b *Bridge) unreachableStream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	if b.unreachable.Load() {
		return status.Error(codes.Unavailable, "bridgetest: the bridge is unreachable")
	}
	return h(srv, ss)
}

// callerCN reads the common name off the caller's client certificate, which is
// the seat identity, so a frame is never fanned back to its sender.
func callerCN(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return ""
	}
	for _, chain := range tlsInfo.State.VerifiedChains {
		if len(chain) > 0 {
			return chain[0].Subject.CommonName
		}
	}
	if len(tlsInfo.State.PeerCertificates) > 0 {
		return tlsInfo.State.PeerCertificates[0].Subject.CommonName
	}
	return ""
}
