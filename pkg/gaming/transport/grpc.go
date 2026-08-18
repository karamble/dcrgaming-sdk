package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/decred/slog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

// The host, over gRPC and mutual TLS.
//
// This game is the client and dials out, so it needs no inbound port, no
// forwarding, and no address the host has to be told - which is what lets it
// run on a laptop behind NAT against a bridge on somebody's appliance.
//
// It never states which game it is. The certificate it presents is its
// identity, and the bridge resolves that: everything the bridge enforces -
// which game a frame may be sent as, which account may be spent from, under
// what caps - hangs off what the certificate resolves to. The credential is
// carried here by hand once and lives on this machine; nothing fetches it.

// BridgeConfig is what a game needs to reach a bridge.
type BridgeConfig struct {
	// Addr is host:port. The bridge is reached by address rather than by a
	// name anything issued, so nothing here checks a hostname.
	Addr string

	// ClientCert and ClientKey are this game's credential, PEM.
	ClientCert, ClientKey []byte

	// BridgeCert is the bridge's own certificate, PEM, which this pins.
	//
	// Pinned rather than verified against a certificate authority: the bridge
	// is somebody's own appliance with a self-signed pair, and the operator
	// carried these bytes here themselves. That makes them a stronger
	// statement about which bridge this is than any CA could make.
	BridgeCert []byte

	// GameID, GameVer and ClientVersion are what Hello introduces this game
	// as; the bridge checks them rather than believing them.
	GameID        string
	GameVer       int32
	ClientVersion string
	Capabilities  []gamingpb.Capability

	// MinRefundBlocks and BondLockBlocks are this game's money-lock terms, told
	// to the bridge so it can mint an invite the game will accept and disclose
	// the durations before a person pays. Zero means the game has no preference.
	MinRefundBlocks uint32
	BondLockBlocks  uint32

	// Log, if set, records connection trouble. Nothing here is fatal, so
	// without it a game reconnecting in a loop does so silently.
	Log slog.Logger

	// OnGap, if set, is called when the bridge says frames were missed, with
	// the tables it named - empty meaning it could not say which.
	//
	// Called only when the bridge declares a gap, never merely because the
	// stream reconnected. A game that resynchronised on its own reconnect
	// loop would pay for one resync per failed dial per table, which is
	// exactly what the bridge declaring it is for.
	OnGap func(gcids []string)
}

// Bridge is a game's connection to a bridge.
type Bridge struct {
	cfg  BridgeConfig
	conn *grpc.ClientConn
	rpc  gamingpb.BridgeServiceClient

	// game is what the bridge said this credential resolves to, learned at
	// Hello. Recorded rather than asserted: it is the bridge's answer.
	game    string
	network string

	frames   chan InboundFrame
	requests chan *gamingpb.BridgeRequest

	// mu guards where the stream has got to, which the next Subscribe sends
	// back so the bridge can tell a clean reconnect from a lossy one.
	mu      sync.Mutex
	epoch   string
	lastSeq uint64
}

// requestBuffer is how many operator requests may queue before one is dropped.
// These are things a person clicked; a game that has not answered a dozen is
// not going to answer the thirteenth.
const requestBuffer = 32

// Dial connects to a bridge and returns once the connection is usable.
func Dial(ctx context.Context, cfg BridgeConfig) (*Bridge, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("a bridge address is needed")
	}
	if cfg.GameID == "" {
		return nil, errors.New("a game id is needed to introduce this game")
	}
	if cfg.GameVer <= 0 {
		return nil, errors.New("a game protocol version is needed to introduce this game")
	}
	if cfg.ClientVersion == "" {
		return nil, errors.New("a client version is needed to introduce this game")
	}
	tlsCfg, err := clientTLS(cfg)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(cfg.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			// The bridge hangs up on anything pinging faster than every
			// ten seconds, so this is comfortably inside that.
			Time:                30 * time.Second,
			Timeout:             20 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("reach the bridge at %s: %w", cfg.Addr, err)
	}

	c := &Bridge{
		cfg:      cfg,
		conn:     conn,
		rpc:      gamingpb.NewBridgeServiceClient(conn),
		frames:   make(chan InboundFrame),
		requests: make(chan *gamingpb.BridgeRequest, requestBuffer),
	}
	return c, nil
}

// clientTLS is what this game presents and what it insists on seeing back.
func clientTLS(cfg BridgeConfig) (*tls.Config, error) {
	pair, err := tls.X509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("load this game's credential: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.BridgeCert) {
		return nil, errors.New("the bridge's certificate did not parse")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
		// The bridge is pinned by its certificate and reached by address, so
		// there is no name to check. The callback below still verifies it
		// chains to the pinned bytes, which is the part that matters.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("the bridge presented no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
				return fmt.Errorf("this is not the bridge whose certificate was configured: %w", err)
			}
			return nil
		},
	}, nil
}

func (c *Bridge) debugf(format string, args ...interface{}) {
	if c.cfg.Log != nil {
		c.cfg.Log.Debugf(format, args...)
	}
}

func (c *Bridge) warnf(format string, args ...interface{}) {
	if c.cfg.Log != nil {
		c.cfg.Log.Warnf(format, args...)
	}
}

// SetLog attaches a logger after construction, which is what the --debug flag
// does once it knows whether it was asked for.
func (c *Bridge) SetLog(l slog.Logger) { c.cfg.Log = l }

// Close ends the connection.
func (c *Bridge) Close() error { return c.conn.Close() }

// Game is what the bridge resolved this credential to, known after Hello.
func (c *Bridge) Game() string { return c.game }

// Network is the chain the bridge is on, known after Hello.
func (c *Bridge) Network() string { return c.network }

// Hello introduces this game and reports what the bridge considers it to be.
//
// The id it sends is advisory and the bridge checks it rather than believing
// it: a mismatch means this credential was copied onto the wrong machine, and
// the bridge refusing loudly is what makes that visible instead of letting the
// game quietly act as something else.
//
// The network is the part a caller must act on. A game built for one chain
// talking to a bridge on another produces scripts nobody can spend and pays
// real money into them, so a mismatch here has to stop the program.
func (c *Bridge) Hello(ctx context.Context, network string) (*gamingpb.HelloReply, error) {
	reply, err := c.rpc.Hello(ctx, &gamingpb.HelloRequest{
		GameId:              c.cfg.GameID,
		GameProtocolVersion: uint32(c.cfg.GameVer),
		ClientVersion:       c.cfg.ClientVersion,
		Capabilities:        c.cfg.Capabilities,
		MinRefundBlocks:     c.cfg.MinRefundBlocks,
		BondLockBlocks:      c.cfg.BondLockBlocks,
	})
	if err != nil {
		return nil, hostErr("introduce this game", err)
	}
	if got := reply.GetNetwork(); got != "" && got != network {
		return nil, fmt.Errorf(
			"this game is set up for %s and the bridge is on %s; "+
				"playing across that would build scripts nobody can spend", network, got)
	}
	c.game, c.network = reply.GetGame(), reply.GetNetwork()
	return reply, nil
}

// hostErr turns a gRPC status into something a person reading a log can act on.
//
// The distinction that matters is between "the bridge said no" and "the bridge
// could not be asked". A game that treated the second as a refusal would stop
// waiting on money that was still going to arrive - which has cost real coin
// here before.
// hostMessage carries a person-readable message while keeping the underlying
// gRPC status reachable, so Unreachable and other code-based callers still work.
// The rendered text is the message alone; Unwrap exposes the original status.
type hostMessage struct {
	msg string
	err error
}

func (h *hostMessage) Error() string { return h.msg }
func (h *hostMessage) Unwrap() error { return h.err }

func hostErr(what string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%s: %w", what, err)
	}
	// Wrap the original status rather than only its message, so the code
	// survives for Unreachable: recording an unreachable bridge as a refusal
	// once meant a stake was paid twice.
	msg := func(text string) error { return &hostMessage{msg: what + ": " + text, err: err} }
	switch st.Code() {
	case codes.Unimplemented:
		// The bridge answers this way when it is switched off, when the
		// credential has been withdrawn, and when it has no gaming in it
		// at all - deliberately the same answer to all three.
		return msg("this bridge is not answering; it may be switched off, " +
			"or this credential may have been revoked")
	case codes.Unavailable:
		return msg("the bridge is not reachable right now: " + st.Message())
	case codes.ResourceExhausted:
		return msg("refused by the spending limit set for this game: " + st.Message())
	case codes.FailedPrecondition:
		return msg(st.Message())
	default:
		return msg(st.Message())
	}
}

// Unreachable reports whether an error means the bridge could not be asked, as
// opposed to having answered.
//
// Callers waiting on money need this: not being able to ask is not an answer,
// and recording it as a refusal once meant a stake was paid twice.
func Unreachable(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Unimplemented:
		return true
	}
	return false
}

// SetOnGap registers what to do when the bridge says frames were missed.
//
// Set after construction because the thing that resynchronises is built from
// this connection, so neither can be the other's argument.
func (c *Bridge) SetOnGap(f func(gcids []string)) { c.cfg.OnGap = f }
