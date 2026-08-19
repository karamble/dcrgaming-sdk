package bridgetest

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
)

func params() stdaddr.AddressParams { return chaincfg.TestNet3Params() }

func payTo(t *testing.T) string {
	t.Helper()
	var h [20]byte
	copy(h[:], "a twenty byte hash..")
	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(h[:], params())
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	return addr.String()
}

func stamp(cfg *transport.BridgeConfig) {
	cfg.GameID = "testgame"
	cfg.GameVer = 1
	cfg.ClientVersion = "bridgetest"
}

// serve stands a bridge up with two seats and returns the first seat's dialed
// connection, which is all most of these need.
func serve(t *testing.T, b *Bridge) (*Server, *transport.Bridge) {
	t.Helper()
	srv, err := b.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	conn, err := srv.Dial(context.Background(), "seat0", stamp)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return srv, conn
}

// The reason this package exists. A bridge nobody can reach must look
// unreachable, not refused - a game that confuses the two pays twice.
func TestAnUnreachableBridgeReadsAsUnreachable(t *testing.T) {
	b := New(Options{Params: params(), Height: 100})
	_, conn := serve(t, b)
	ctx := context.Background()

	b.SetUnreachable(true)
	_, err := conn.SpendStatus(ctx, "spend-1")
	if err == nil {
		t.Fatal("an unreachable bridge answered")
	}
	if !transport.Unreachable(err) {
		t.Fatalf("an unreachable bridge did not read as unreachable: %v", err)
	}
}

// And it must be a passing condition, not a permanent one: the whole point is
// that a game keeps asking until the bridge comes back.
func TestAnUnreachableBridgeComesBack(t *testing.T) {
	b := New(Options{Params: params(), Height: 100})
	_, conn := serve(t, b)
	ctx := context.Background()

	b.SetUnreachable(true)
	if _, err := conn.ChainTip(ctx); err == nil {
		t.Fatal("an unreachable bridge answered")
	}
	b.SetUnreachable(false)
	tip, err := conn.ChainTip(ctx)
	if err != nil {
		t.Fatalf("the bridge did not come back: %v", err)
	}
	if tip.Height != 100 {
		t.Fatalf("tip is %d, want 100", tip.Height)
	}
}

// A held request is what a real bridge looks like while a person decides. It
// must not settle by itself, and it must settle when they answer.
func TestAHeldSpendStaysPendingUntilAnswered(t *testing.T) {
	b := New(Options{Params: params(), Height: 100})
	_, conn := serve(t, b)
	ctx := context.Background()
	b.SetVerdict(Hold, "")

	sp, err := conn.RequestSpend(ctx, payTo(t), 5_000_000, "a stake")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if sp.Settled() {
		t.Fatal("a held request settled on its own")
	}
	got, err := conn.SpendStatus(ctx, sp.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got.Settled() {
		t.Fatal("a held request settled while nobody had answered")
	}

	if !b.Settle(sp.ID, Approve, "") {
		t.Fatal("there was no request to answer")
	}
	got, err = conn.SpendStatus(ctx, sp.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got.State != transport.SpendApproved || got.TxID == "" {
		t.Fatalf("after approval the state is %q with txid %q", got.State, got.TxID)
	}
}

// A refusal is the opposite of unreachable: the bridge answered, and the answer
// was no. Terminal, and it must carry its reason.
func TestARefusedSpendIsTerminalAndKeepsItsReason(t *testing.T) {
	b := New(Options{Params: params(), Height: 100})
	_, conn := serve(t, b)
	b.SetVerdict(Refuse, "over the cap")

	sp, err := conn.RequestSpend(context.Background(), payTo(t), 5_000_000, "a stake")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if !sp.Settled() {
		t.Fatal("a refusal did not settle")
	}
	if sp.State != transport.SpendDenied {
		t.Fatalf("state is %q, want %q", sp.State, transport.SpendDenied)
	}
	if sp.Error != "over the cap" {
		t.Fatalf("the refusal lost its reason: %q", sp.Error)
	}
}

// An approved spend has to become an output a game can actually find, or the
// fund path cannot be tested at all.
func TestAnApprovedSpendBecomesAFindableOutput(t *testing.T) {
	b := New(Options{Params: params(), Height: 100})
	_, conn := serve(t, b)
	ctx := context.Background()

	sp, err := conn.RequestSpend(ctx, payTo(t), 5_000_000, "a stake")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if sp.State != transport.SpendApproved || sp.TxID == "" {
		t.Fatalf("approve is the default, got %q", sp.State)
	}

	// Not confirmed yet, so a confirmed-only lookup must not see it.
	out, err := conn.Outpoint(ctx, sp.TxID, 1)
	if err != nil {
		t.Fatalf("outpoint: %v", err)
	}
	if out.Found {
		t.Fatal("an unconfirmed output was visible to a confirmed-only lookup")
	}
	if out, err = conn.UnconfirmedOutpoint(ctx, sp.TxID, 1); err != nil || !out.Found {
		t.Fatalf("an unconfirmed lookup did not find it: found=%v err=%v", out.Found, err)
	}

	b.Mine(1)
	out, err = conn.Outpoint(ctx, sp.TxID, 1)
	if err != nil {
		t.Fatalf("outpoint: %v", err)
	}
	if !out.Found || out.ValueAtoms != 5_000_000 {
		t.Fatalf("after a block: found=%v value=%d", out.Found, out.ValueAtoms)
	}
}

// One seat's frame reaches the other and never comes back to the sender, which
// is the routing property the real bridge provides by certificate identity.
func TestAFrameReachesTheOtherSeatAndNotItself(t *testing.T) {
	b := New(Options{Params: params(), Height: 100})
	srv, seat0 := serve(t, b)
	ctx := context.Background()
	seat1, err := srv.Dial(ctx, "seat1", stamp)
	if err != nil {
		t.Fatalf("dial seat1: %v", err)
	}

	in0, err := seat0.Events(ctx)
	if err != nil {
		t.Fatalf("events seat0: %v", err)
	}
	in1, err := seat1.Events(ctx)
	if err != nil {
		t.Fatalf("events seat1: %v", err)
	}
	// Let both subscriptions register before anything is sent.
	deadline := time.Now().Add(2 * time.Second)
	for b.Subscribers() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if err := seat0.SendGC(ctx, "gc1", "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case f := <-in1:
		if string(f.Frame) != "hello" {
			t.Fatalf("seat1 got %q", string(f.Frame))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("seat1 never received the frame")
	}
	select {
	case <-in0:
		t.Fatal("a frame was fanned back to its sender")
	case <-time.After(150 * time.Millisecond):
	}
}

// Broadcast is taken at its word, and every output it carries has to land, or a
// settlement cannot be followed to its outputs.
func TestBroadcastPutsEveryOutputOnTheChain(t *testing.T) {
	b := New(Options{Params: params(), Height: 100})
	_, conn := serve(t, b)
	ctx := context.Background()

	addr, err := stdaddr.DecodeAddress(payTo(t), params())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, pkScript := addr.PaymentScript()
	raw := twoOutputTx(t, pkScript)

	txid, err := conn.Broadcast(ctx, raw)
	if err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if got := b.SentCount(txid); got != 1 {
		t.Fatalf("sent count is %d, want 1", got)
	}
	for vout := range uint32(2) {
		if _, ok := b.Output(txid, vout); !ok {
			t.Fatalf("output %d did not land", vout)
		}
	}
}

func TestServeRefusesABridgeWithNoSeats(t *testing.T) {
	if _, err := New(Options{Params: params()}).Serve(); err == nil {
		t.Fatal("served a bridge nobody could connect to")
	}
}

func TestDialRefusesASeatThatWasNeverIssuedACertificate(t *testing.T) {
	b := New(Options{Params: params(), Height: 100})
	srv, _ := serve(t, b)
	if _, err := srv.Dial(context.Background(), "seat9", stamp); err == nil {
		t.Fatal("dialed as a seat with no certificate")
	}
}

// The block clock is deterministic so two seats reading one height agree, the
// way they would against a real chain.
func TestTheBlockClockIsDeterministicAndPerHeight(t *testing.T) {
	if BlockHashHex(7) != BlockHashHex(7) {
		t.Fatal("the same height hashed two ways")
	}
	if BlockHashHex(7) == BlockHashHex(8) {
		t.Fatal("two heights share a hash")
	}
}

// twoOutputTx is a transaction with two outputs and no real inputs. The fake
// takes a broadcast at its word, so it needs to be well formed, not spendable.
func twoOutputTx(t *testing.T, pkScript []byte) string {
	t.Helper()
	tx := wire.NewMsgTx()
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 0}, 0, nil))
	tx.AddTxOut(wire.NewTxOut(1_000, pkScript))
	tx.AddTxOut(wire.NewTxOut(2_000, pkScript))
	raw, err := tx.Bytes()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return hex.EncodeToString(raw)
}
