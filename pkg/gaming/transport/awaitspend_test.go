package transport

import (
	"context"
	"crypto/x509"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

// spendFake answers SpendStatus unreachable a few times, then approves.
type spendFake struct {
	gamingpb.UnimplementedBridgeServiceServer
	unreachableFor int32
	asked          atomic.Int32
}

func (f *spendFake) Hello(context.Context, *gamingpb.HelloRequest) (*gamingpb.HelloReply, error) {
	return &gamingpb.HelloReply{Game: "g", Network: "mainnet"}, nil
}

func (f *spendFake) SpendStatus(_ context.Context, req *gamingpb.SpendStatusRequest) (*gamingpb.Spend, error) {
	if f.asked.Add(1) <= f.unreachableFor {
		return nil, status.Error(codes.Unavailable, "the bridge went away")
	}
	return &gamingpb.Spend{Id: req.GetId(), State: string(SpendApproved), Txid: "abc"}, nil
}

// The failure that cost 0.01 DCR on mainnet: giving up on the first refused
// connection turned a bridge restart into a payment made twice.
func TestAwaitSpendKeepsAskingAcrossAnUnreachableBridge(t *testing.T) {
	old := spendPoll
	spendPoll = time.Millisecond
	t.Cleanup(func() { spendPoll = old })

	f := &spendFake{unreachableFor: 3}
	c := dialSpendFake(t, f)

	sp, err := c.AwaitSpend(context.Background(), "s1")
	if err != nil {
		t.Fatalf("await gave up while the bridge was away: %v", err)
	}
	if sp.State != SpendApproved || sp.TxID != "abc" {
		t.Fatalf("state=%q txid=%q", sp.State, sp.TxID)
	}
	if got := f.asked.Load(); got < 4 {
		t.Fatalf("asked %d times, want at least 4 - it did not keep asking", got)
	}
}

// An error that is not unreachable is a real failure and must come back.
func TestAwaitSpendReturnsAnErrorItDoesNotUnderstand(t *testing.T) {
	old := spendPoll
	spendPoll = time.Millisecond
	t.Cleanup(func() { spendPoll = old })

	c := dialSpendFake(t, &brokenSpendFake{})
	if _, err := c.AwaitSpend(context.Background(), "s1"); err == nil {
		t.Fatal("a real failure was swallowed and waited on forever")
	}
}

type brokenSpendFake struct {
	gamingpb.UnimplementedBridgeServiceServer
}

func (f *brokenSpendFake) Hello(context.Context, *gamingpb.HelloRequest) (*gamingpb.HelloReply, error) {
	return &gamingpb.HelloReply{Game: "g", Network: "mainnet"}, nil
}

func (f *brokenSpendFake) SpendStatus(context.Context, *gamingpb.SpendStatusRequest) (*gamingpb.Spend, error) {
	return nil, status.Error(codes.InvalidArgument, "no such request")
}

// A caller's own deadline still ends the wait, so an unreachable bridge cannot
// hang a game forever.
func TestAwaitSpendStillHonoursTheCallersDeadline(t *testing.T) {
	old := spendPoll
	spendPoll = time.Millisecond
	t.Cleanup(func() { spendPoll = old })

	c := dialSpendFake(t, &spendFake{unreachableFor: 1 << 30})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.AwaitSpend(ctx, "s1"); err == nil {
		t.Fatal("the wait outlived its context")
	}
}

// dialSpendFake stands any bridge implementation up and dials it, which the
// existing dialFake cannot do because it is typed to one fake.
func dialSpendFake(t *testing.T, impl gamingpb.BridgeServiceServer) *Bridge {
	t.Helper()

	serverCert, serverKey := selfSigned(t, "bridge")
	clientCert, clientKey := selfSigned(t, "poker")

	pair, err := tlsPair(serverCert, serverKey)
	if err != nil {
		t.Fatalf("load the bridge's pair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(clientCert) {
		t.Fatal("the game's certificate did not parse")
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS(pair, pool))))
	gamingpb.RegisterBridgeServiceServer(srv, impl)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := Dial(context.Background(), BridgeConfig{
		Addr: lis.Addr().String(), ClientCert: clientCert, ClientKey: clientKey,
		BridgeCert: serverCert, GameID: "poker", GameVer: 5, ClientVersion: "dcrpoker",
	})
	if err != nil {
		t.Fatalf("dial the bridge: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
