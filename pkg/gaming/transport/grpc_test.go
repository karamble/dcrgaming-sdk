package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

// A real bridge, on a real socket, behind real mutual TLS.
//
// Not a stub: the things worth asserting here are that this game presents a
// credential and states nothing about who it is, that a refusal survives the
// wire with its reason, and that it resynchronises when and only when the
// bridge says to. A fake transport would assert none of those.

// fakeBridge is a bridge that answers however a test needs it to.
type fakeBridge struct {
	gamingpb.UnimplementedBridgeServiceServer

	game    string
	network string

	// sent records what the game asked to send, so a test can look at what
	// crossed the wire rather than at what it meant to put there.
	sent []*gamingpb.SendFrameRequest

	// start is the opening event of every stream, and events is what follows.
	start  *gamingpb.StreamStart
	events []*gamingpb.BridgeEvent

	// refuse, if set, is returned by SendFrame.
	refuse error

	// hello records what the game introduced itself as.
	hello *gamingpb.HelloRequest
}

func (f *fakeBridge) Hello(_ context.Context, req *gamingpb.HelloRequest) (*gamingpb.HelloReply, error) {
	f.hello = req
	return &gamingpb.HelloReply{Game: f.game, Network: f.network}, nil
}

func (f *fakeBridge) SendFrame(_ context.Context, req *gamingpb.SendFrameRequest) (*gamingpb.SendFrameReply, error) {
	if f.refuse != nil {
		return nil, f.refuse
	}
	f.sent = append(f.sent, req)
	return &gamingpb.SendFrameReply{}, nil
}

func (f *fakeBridge) Subscribe(_ *gamingpb.SubscribeRequest, stream grpc.ServerStreamingServer[gamingpb.BridgeEvent]) error {
	start := f.start
	if start == nil {
		start = &gamingpb.StreamStart{Epoch: "e1"}
	}
	if err := stream.Send(&gamingpb.BridgeEvent{
		Event: &gamingpb.BridgeEvent_Start{Start: start},
	}); err != nil {
		return err
	}
	for _, ev := range f.events {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

// dialFake stands the bridge up and connects a game to it, the way an operator
// would: the game holds a credential and the bridge's certificate to pin.
func dialFake(t *testing.T, f *fakeBridge, onGap func([]string)) *Bridge {
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
	gamingpb.RegisterBridgeServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := Dial(context.Background(), BridgeConfig{
		Addr:          lis.Addr().String(),
		ClientCert:    clientCert,
		ClientKey:     clientKey,
		BridgeCert:    serverCert,
		GameID:        "poker",
		GameVer:       5,
		ClientVersion: "dcrpoker",
		OnGap:         onGap,
	})
	if err != nil {
		t.Fatalf("dial the bridge: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Dial refuses a config that never introduced its game. The identity fields
// are optional struct fields the compiler cannot miss for a caller, so the
// refusal is what turns a forgotten stamp into a loud failure.
func TestDialRefusesAnUnstampedIdentity(t *testing.T) {
	serverCert, _ := selfSigned(t, "bridge")
	clientCert, clientKey := selfSigned(t, "poker")
	base := BridgeConfig{
		Addr:          "127.0.0.1:1",
		ClientCert:    clientCert,
		ClientKey:     clientKey,
		BridgeCert:    serverCert,
		GameID:        "poker",
		GameVer:       5,
		ClientVersion: "dcrpoker",
	}
	if c, err := Dial(context.Background(), base); err != nil {
		t.Fatalf("the fully stamped config should dial: %v", err)
	} else {
		_ = c.Close()
	}
	for name, breakIt := range map[string]func(*BridgeConfig){
		"no game id":        func(c *BridgeConfig) { c.GameID = "" },
		"no game version":   func(c *BridgeConfig) { c.GameVer = 0 },
		"no client version": func(c *BridgeConfig) { c.ClientVersion = "" },
	} {
		cfg := base
		breakIt(&cfg)
		if _, err := Dial(context.Background(), cfg); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}

// This game never states which game it is.
//
// The certificate is the identity. A request that also carried a name would let
// a game claim another's - and everything the bridge enforces, from which
// frames route to which account pays, hangs off which game it decided this is.
func TestTheGameNeverNamesItselfOnTheWire(t *testing.T) {
	f := &fakeBridge{game: "poker", network: "mainnet"}
	c := dialFake(t, f, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.SendGC(ctx, "aa", "--gaming[..]--QUJD"); err != nil {
		t.Fatalf("send a frame: %v", err)
	}
	if len(f.sent) != 1 {
		t.Fatalf("the bridge received %d frames", len(f.sent))
	}
	// The generated request type has no field for it at all, which is the
	// strongest form this can take: it is not that the game declines to say,
	// it is that the wire gives it nowhere to say it.
	if got := f.sent[0].ProtoReflect().Descriptor().Fields().ByName("game"); got != nil {
		t.Fatal("the send request has a game field, so a game could claim to be another")
	}
}

// A refusal arrives with its reason.
//
// The person who has to act on it is reading a log. "The bridge said no" with
// nothing after it sends them to the wrong place, and the bridge's refusals are
// mostly things they can fix.
func TestARefusalKeepsItsReason(t *testing.T) {
	f := &fakeBridge{
		game:   "poker",
		refuse: status.Error(codes.ResourceExhausted, "over the per-day cap for poker"),
	}
	c := dialFake(t, f, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.SendGC(ctx, "aa", "--gaming[..]--QUJD")
	if err == nil {
		t.Fatal("a refused send reported success")
	}
	if !contains(err.Error(), "per-day cap") {
		t.Fatalf("the refusal lost its reason on the way: %v", err)
	}
}

// The game resynchronises when the bridge says to, and not otherwise.
//
// This is the whole of the gap contract. Resynchronising on its own reconnect
// loop would cost one resync per failed dial per table; not resynchronising
// when frames really were lost leaves the table disagreeing with its peers.
func TestAGapIsDeclaredByTheBridgeAndNobodyElse(t *testing.T) {
	gaps := make(chan []string, 4)
	f := &fakeBridge{
		game: "poker",
		start: &gamingpb.StreamStart{
			Epoch: "e1", Gap: true,
			GapScope: gamingpb.GapScope_GAP_SCOPED,
			GapGcids: []string{"table-one"},
		},
	}
	c := dialFake(t, f, func(gcids []string) { gaps <- gcids })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.Events(ctx); err != nil {
		t.Fatalf("open the stream: %v", err)
	}

	select {
	case got := <-gaps:
		if len(got) != 1 || got[0] != "table-one" {
			t.Fatalf("the gap named %v, not the table the bridge said", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge declared a gap and the game never heard about it, so it will act " +
			"on a table state it never received")
	}
}

func TestNoGapMeansNoResync(t *testing.T) {
	gaps := make(chan []string, 4)
	f := &fakeBridge{
		game:  "poker",
		start: &gamingpb.StreamStart{Epoch: "e1"},
		// A frame queued behind the opening event. Everything below is
		// sequenced against its arrival rather than against a clock.
		events: []*gamingpb.BridgeEvent{{
			Event: &gamingpb.BridgeEvent_Frame{Frame: &gamingpb.Frame{
				Seq: 1, Gcid: "table-one", From: "bob", Frame: "--gaming[..]--QUJD",
			}},
		}},
	}
	c := dialFake(t, f, func(gcids []string) { gaps <- gcids })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, err := c.Events(ctx)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}

	// The frame cannot arrive before the opening event was handled, because
	// they cross the same stream in that order. So once it is here, whatever
	// the opening event was going to do has been done - and asking about the
	// gap channel is a question about the past rather than a race with it.
	select {
	case got := <-frames:
		if got.GCID != "table-one" {
			t.Fatalf("the stream delivered %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing came off the stream at all, so this asserts nothing")
	}

	select {
	case <-gaps:
		t.Fatal("a stream that missed nothing told the game to resynchronise, so a brief " +
			"outage would cost a resync per reconnect attempt per table")
	default:
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// selfSigned mints a throwaway pair, the shape the operator would carry by hand.
func selfSigned(t *testing.T, name string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("mint a certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// tlsPair and serverTLS are the bridge side of the handshake, which this test
// stands up so the client half is exercised against something real.
func tlsPair(certPEM, keyPEM []byte) (tls.Certificate, error) {
	return tls.X509KeyPair(certPEM, keyPEM)
}

func serverTLS(cert tls.Certificate, clients *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clients,
	}
}

// Hello sends the configured identity, not a constant. The fixture's values
// are deliberately not poker's, so a hardcoded identity cannot pass.
func TestHelloIntroducesTheConfiguredIdentity(t *testing.T) {
	serverCert, serverKey := selfSigned(t, "bridge")
	clientCert, clientKey := selfSigned(t, "game")

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
	f := &fakeBridge{game: "battleships", network: "mainnet"}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS(pair, pool))))
	gamingpb.RegisterBridgeServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := Dial(context.Background(), BridgeConfig{
		Addr:          lis.Addr().String(),
		ClientCert:    clientCert,
		ClientKey:     clientKey,
		BridgeCert:    serverCert,
		GameID:        "battleships",
		GameVer:       9,
		ClientVersion: "shipyard",
		Capabilities:  []gamingpb.Capability{gamingpb.Capability_CAP_RECLAIM},
	})
	if err != nil {
		t.Fatalf("dial the bridge: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Hello(ctx, "mainnet"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if f.hello == nil {
		t.Fatal("the bridge never received a hello")
	}
	if got := f.hello.GetGameId(); got != "battleships" {
		t.Errorf("hello introduced %q, want the configured id", got)
	}
	if got := f.hello.GetGameProtocolVersion(); got != 9 {
		t.Errorf("hello spoke version %d, want the configured 9", got)
	}
	if got := f.hello.GetClientVersion(); got != "shipyard" {
		t.Errorf("hello named client %q, want the configured one", got)
	}
	caps := f.hello.GetCapabilities()
	if len(caps) != 1 || caps[0] != gamingpb.Capability_CAP_RECLAIM {
		t.Errorf("hello carried capabilities %v, want the configured single one", caps)
	}
}
