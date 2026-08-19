// Package connect is a game's connection to a dcrpulse gaming bridge: what it
// is called, where the bridge is, and the credential that proves who is
// calling.
//
// It is headless on purpose. Both games grew a first-run wizard that prompts on
// a terminal, and those wizards are 91% the same code - but the 9% is where a
// game decides how it talks to its operator, and a game with a settings screen,
// a config manager or no terminal at all should not be handed a prompt loop it
// has to work around. So this package reads, writes, stamps and proves; asking
// a person anything is the game's own business.
//
// # The four files
//
// A connected game keeps four things beside each other in its data directory:
// bridge.json for the address and the chain, and client.cert, client.key and
// bridge.cert for the credential. They are separate files rather than one
// because the credential is copied across by hand from the dashboard, and a
// person pasting PEM into a JSON string gets it wrong.
//
// Written 0600 in a 0700 directory, through a temporary name and a rename, so a
// crash mid-write cannot leave a game holding half a credential.
//
// # Why the chain is stored and checked
//
// bridge.json records which chain the game was set up against, and [Prove]
// refuses a bridge that answers with a different one. A mismatch is not a
// warning: escrow scripts built for one chain are addresses nobody can spend on
// the other, so the money would go in and not come out. Refusing to start is
// the kind thing to do.
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
)

// File names in a game's data directory.
const (
	ConfigFile     = "bridge.json"
	ClientCertFile = "client.cert"
	ClientKeyFile  = "client.key"
	BridgeCertFile = "bridge.cert"
)

// ErrNotConnected means this game has never been pointed at a bridge. It is
// the signal for a game to run whatever first-run flow it prefers, and then
// call [Save].
var ErrNotConnected = errors.New("this game has not been connected to a bridge yet")

// proveTimeout bounds the check that a bridge answers. A variable only so a
// test need not wait.
var proveTimeout = 30 * time.Second

// Identity is what a game introduces itself as.
//
// Dial refuses a config that never passed through [Stamp], which is deliberate:
// the identity fields are optional-looking struct fields that a caller can
// silently forget, so forgetting them is made loud rather than left to show up
// as a bridge routing frames to nobody.
type Identity struct {
	// GameID and GameVer are what the bridge routes on. The bridge checks
	// them rather than believing them.
	GameID  string
	GameVer int32
	// ClientVersion is this program's own name and version, for the operator
	// to read in the dashboard.
	ClientVersion string
	// Capabilities are the requests the dashboard may offer for this game.
	// RefreshState needs none and is always available.
	Capabilities []gamingpb.Capability
	// MinRefundBlocks and BondLockBlocks are the game's money-lock terms,
	// told to the bridge so it mints an invite the game will accept and can
	// disclose the durations before a person pays.
	MinRefundBlocks uint32
	BondLockBlocks  uint32
}

// Validate reports whether the identity says enough to introduce a game.
func (id Identity) Validate() error {
	if id.GameID == "" {
		return fmt.Errorf("a game must say what it is called")
	}
	if id.GameVer <= 0 {
		return fmt.Errorf("a game must say which version of its protocol it speaks")
	}
	return nil
}

// Stamp fills in what Hello introduces this game as.
func Stamp(cfg *transport.BridgeConfig, id Identity) {
	cfg.GameID = id.GameID
	cfg.GameVer = id.GameVer
	cfg.ClientVersion = id.ClientVersion
	cfg.Capabilities = id.Capabilities
	cfg.MinRefundBlocks = id.MinRefundBlocks
	cfg.BondLockBlocks = id.BondLockBlocks
}

// stored is the JSON shape of bridge.json.
type stored struct {
	Addr    string `json:"addr"`
	Network string `json:"network"`
}

// Load reads a stored connection and stamps the game's identity onto it.
//
// Returns [ErrNotConnected] if the game has never been connected, which is the
// game's cue to ask its operator however it likes.
func Load(dir string, id Identity) (transport.BridgeConfig, string, error) {
	if err := id.Validate(); err != nil {
		return transport.BridgeConfig{}, "", err
	}
	raw, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if os.IsNotExist(err) {
		return transport.BridgeConfig{}, "", ErrNotConnected
	}
	if err != nil {
		return transport.BridgeConfig{}, "", err
	}
	var s stored
	if err := json.Unmarshal(raw, &s); err != nil {
		return transport.BridgeConfig{}, "", fmt.Errorf("read %s: %w", ConfigFile, err)
	}

	cfg := transport.BridgeConfig{Addr: s.Addr}
	for _, f := range []struct {
		name string
		into *[]byte
	}{
		{ClientCertFile, &cfg.ClientCert},
		{ClientKeyFile, &cfg.ClientKey},
		{BridgeCertFile, &cfg.BridgeCert},
	} {
		b, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			return transport.BridgeConfig{}, "", fmt.Errorf("read %s: %w", f.name, err)
		}
		*f.into = b
	}
	Stamp(&cfg, id)
	return cfg, s.Network, nil
}

// Network reports which chain this game was configured for, or "" if it has not
// been configured. A game compares this against whatever its own flag says
// before it builds a single script.
func Network(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return ""
	}
	var s stored
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s.Network
}

// Save writes the connection and the credential beside each other.
func Save(dir string, cfg transport.BridgeConfig, network string) error {
	if cfg.Addr == "" {
		return fmt.Errorf("a connection must say where the bridge is")
	}
	if network == "" {
		return fmt.Errorf("a connection must say which chain it is for")
	}
	if len(cfg.ClientCert) == 0 || len(cfg.ClientKey) == 0 || len(cfg.BridgeCert) == 0 {
		return fmt.Errorf("a connection needs the game's credential and the bridge's certificate")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(stored{Addr: cfg.Addr, Network: network}, "", "  ")
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		body []byte
	}{
		{ClientCertFile, cfg.ClientCert},
		{ClientKeyFile, cfg.ClientKey},
		{BridgeCertFile, cfg.BridgeCert},
		{ConfigFile, append(body, '\n')},
	} {
		path := filepath.Join(dir, f.name)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, f.body, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
}

// Prove dials the bridge, introduces the game, and hangs up.
//
// Called before [Save] so a connection is only written down once it is known to
// work, and callable afterwards as a health check. The chain is checked here
// rather than left to be discovered later: scripts built for the wrong chain
// are unspendable, so a mismatch has to stop the game before it builds one.
func Prove(ctx context.Context, cfg transport.BridgeConfig, network string) error {
	if network == "" {
		return fmt.Errorf("proving a connection needs the chain to check against")
	}
	ctx, cancel := context.WithTimeout(ctx, proveTimeout)
	defer cancel()

	b, err := transport.Dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer b.Close()

	reply, err := b.Hello(ctx, network)
	if err != nil {
		return err
	}
	if reply.GetGame() == "" {
		return fmt.Errorf("the bridge did not say what this game is")
	}
	// The chain is checked by Hello itself, which refuses a bridge on a
	// different one before this ever sees the reply.
	return nil
}
