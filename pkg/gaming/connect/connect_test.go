package connect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
)

func ident() Identity {
	return Identity{
		GameID: "battleships", GameVer: 1, ClientVersion: "dcrbattleshipsd",
		Capabilities:    []gamingpb.Capability{gamingpb.Capability_CAP_ACCEPT_INVITE},
		MinRefundBlocks: 2048, BondLockBlocks: 4032,
	}
}

func TestAGameThatHasNeverConnectedSaysSo(t *testing.T) {
	_, _, err := Load(t.TempDir(), ident())
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
}

func TestAConnectionSurvivesBeingWrittenAndReadBack(t *testing.T) {
	dir := t.TempDir()
	cfg := transport.BridgeConfig{
		Addr: "127.0.0.1:9999", ClientCert: []byte("cert"),
		ClientKey: []byte("key"), BridgeCert: []byte("bridge"),
	}
	if err := Save(dir, cfg, "mainnet"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, network, err := Load(dir, ident())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if network != "mainnet" || got.Addr != cfg.Addr {
		t.Fatalf("network=%q addr=%q", network, got.Addr)
	}
	if string(got.ClientKey) != "key" || string(got.BridgeCert) != "bridge" {
		t.Fatal("the credential did not come back")
	}
	if Network(dir) != "mainnet" {
		t.Fatalf("Network says %q", Network(dir))
	}
}

// Load stamps the identity, because Dial refuses a config that never did and a
// game that had to remember would eventually forget.
func TestLoadStampsTheGamesIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := transport.BridgeConfig{
		Addr: "127.0.0.1:1", ClientCert: []byte("c"), ClientKey: []byte("k"), BridgeCert: []byte("b"),
	}
	if err := Save(dir, cfg, "mainnet"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _, err := Load(dir, ident())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.GameID != "battleships" || got.GameVer != 1 {
		t.Fatalf("identity not stamped: %q v%d", got.GameID, got.GameVer)
	}
	if got.BondLockBlocks != 4032 || got.MinRefundBlocks != 2048 {
		t.Fatalf("lock terms not stamped: bond=%d refund=%d", got.BondLockBlocks, got.MinRefundBlocks)
	}
	if len(got.Capabilities) != 1 {
		t.Fatalf("capabilities not stamped: %v", got.Capabilities)
	}
}

func TestAnIdentityMustSayWhatTheGameIs(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   Identity
	}{
		{"no name", Identity{GameVer: 1}},
		{"no protocol version", Identity{GameID: "g"}},
		{"a zero version", Identity{GameID: "g", GameVer: 0}},
	} {
		if err := tc.id.Validate(); err == nil {
			t.Errorf("%s: accepted an identity that introduces nothing", tc.name)
		}
	}
}

func TestSaveRefusesAHalfConnection(t *testing.T) {
	full := transport.BridgeConfig{
		Addr: "a", ClientCert: []byte("c"), ClientKey: []byte("k"), BridgeCert: []byte("b"),
	}
	for _, tc := range []struct {
		name    string
		mut     func(*transport.BridgeConfig)
		network string
	}{
		{"no address", func(c *transport.BridgeConfig) { c.Addr = "" }, "mainnet"},
		{"no chain", func(c *transport.BridgeConfig) {}, ""},
		{"no client certificate", func(c *transport.BridgeConfig) { c.ClientCert = nil }, "mainnet"},
		{"no client key", func(c *transport.BridgeConfig) { c.ClientKey = nil }, "mainnet"},
		{"no bridge certificate", func(c *transport.BridgeConfig) { c.BridgeCert = nil }, "mainnet"},
	} {
		cfg := full
		tc.mut(&cfg)
		if err := Save(t.TempDir(), cfg, tc.network); err == nil {
			t.Errorf("%s: wrote a connection that cannot be used", tc.name)
		}
	}
}

// Everything written is owner-only, in an owner-only directory. It is a
// credential.
func TestTheCredentialIsWrittenOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg := transport.BridgeConfig{
		Addr: "a", ClientCert: []byte("c"), ClientKey: []byte("k"), BridgeCert: []byte("b"),
	}
	if err := Save(dir, cfg, "mainnet"); err != nil {
		t.Fatalf("save: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("the data directory is mode %o, want 700", perm)
	}
	for _, name := range []string{ConfigFile, ClientCertFile, ClientKeyFile, BridgeCertFile} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s is mode %o, want 600", name, perm)
		}
	}
}

func TestProveAcceptsABridgeThatAnswers(t *testing.T) {
	b := bridgetest.New(bridgetest.Options{Game: "battleships", Network: "mainnet"})
	srv, err := b.Serve("seat0")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	cfg, err := srv.Config("seat0")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	Stamp(&cfg, ident())
	if err := Prove(context.Background(), cfg, "mainnet"); err != nil {
		t.Fatalf("prove: %v", err)
	}
}

// A bridge on the wrong chain must stop the game before it builds a script
// nobody can spend. The refusal itself is transport.Hello's; this pins the
// behaviour at the boundary a game sees, so a Prove that stopped calling Hello
// with the network would be caught here.
func TestProveRefusesTheWrongChain(t *testing.T) {
	b := bridgetest.New(bridgetest.Options{Game: "battleships", Network: "testnet3"})
	srv, err := b.Serve("seat0")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	cfg, err := srv.Config("seat0")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	Stamp(&cfg, ident())
	err = Prove(context.Background(), cfg, "mainnet")
	if err == nil {
		t.Fatal("accepted a bridge on a different chain than the game was set up for")
	}
}

func TestProveNeedsAChainToCheckAgainst(t *testing.T) {
	if err := Prove(context.Background(), transport.BridgeConfig{}, ""); err == nil {
		t.Fatal("proved a connection against no chain at all")
	}
}

func TestProveFailsOnABridgeThatIsNotThere(t *testing.T) {
	old := proveTimeout
	proveTimeout = 300 * time.Millisecond
	t.Cleanup(func() { proveTimeout = old })

	b := bridgetest.New(bridgetest.Options{Game: "g", Network: "mainnet"})
	srv, err := b.Serve("seat0")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	cfg, err := srv.Config("seat0")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv.Close()
	Stamp(&cfg, ident())
	if err := Prove(context.Background(), cfg, "mainnet"); err == nil {
		t.Fatal("proved a connection to a bridge that had stopped")
	}
}
