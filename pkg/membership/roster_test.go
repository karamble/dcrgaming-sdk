package membership

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

const testCSVBlocks = 64

func testParams() *chaincfg.Params { return chaincfg.SimNetParams() }

func members(t *testing.T, n int) []Member {
	t.Helper()
	out := make([]Member, 0, n)
	for i := range n {
		priv, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		out = append(out, Member{
			Seat:       uint32(i),
			CompPubkey: priv.PubKey().SerializeCompressed(),
			CSVBlocks:  testCSVBlocks,
		})
	}
	return out
}

// A seat's deposit address must not depend on the order the seats happened to
// be collected in. Callers build the member list by ranging a map, so if this
// were order-sensitive two members of one table would derive different
// addresses for the same seat and the money would go to two places.
func TestTheSameMembershipAlwaysDerivesTheSameDeposits(t *testing.T) {
	ms := members(t, 6)
	_, want, err := Close(ms, testParams())
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	reversed := make([]Member, len(ms))
	for i, m := range ms {
		reversed[len(ms)-1-i] = m
	}
	_, got, err := Close(reversed, testParams())
	if err != nil {
		t.Fatalf("close reversed: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("got %d deposits, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seat %d differs:\n got %+v\nwant %+v", want[i].Seat, got[i], want[i])
		}
	}
}

// Deposits come back in seat order whatever order the members arrived in, so a
// caller can index them without re-sorting and without depending on map order.
func TestDepositsComeBackInSeatOrder(t *testing.T) {
	ms := members(t, 4)
	ms[0].Seat, ms[3].Seat = 3, 0

	_, deposits, err := Close(ms, testParams())
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	for i, d := range deposits {
		if d.Seat != uint32(i) {
			t.Fatalf("deposit %d is for seat %d, want seat %d", i, d.Seat, i)
		}
	}
}

// The whole point of the package: what each member's script commits to is the
// whole table, in the one canonical order every member derives independently.
func TestEveryDepositScriptNamesTheWholeTable(t *testing.T) {
	ms := members(t, 5)
	canonical, deposits, err := Close(ms, testParams())
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	keys := make([][]byte, 0, len(ms))
	for _, m := range ms {
		keys = append(keys, m.CompPubkey)
	}
	wantCanonical, err := escrow.CanonicalMembers(keys)
	if err != nil {
		t.Fatalf("canonical members: %v", err)
	}
	if len(canonical) != len(wantCanonical) {
		t.Fatalf("got %d canonical members, want %d", len(canonical), len(wantCanonical))
	}
	for i := range wantCanonical {
		if !bytes.Equal(canonical[i], wantCanonical[i]) {
			t.Fatalf("canonical member %d differs", i)
		}
	}

	// Read the roster back out of each script it produced. A script that
	// named a different set would settle against signatures nobody can
	// supply.
	for _, d := range deposits {
		redeem, err := hex.DecodeString(d.RedeemScriptHex)
		if err != nil {
			t.Fatalf("seat %d: redeem script is not hex: %v", d.Seat, err)
		}
		got, err := escrow.Members(redeem)
		if err != nil {
			t.Fatalf("seat %d: read members back: %v", d.Seat, err)
		}
		if len(got) != len(wantCanonical) {
			t.Fatalf("seat %d names %d members, want %d", d.Seat, len(got), len(wantCanonical))
		}
		for i := range wantCanonical {
			if !bytes.Equal(got[i], wantCanonical[i]) {
				t.Fatalf("seat %d names a different member at %d", d.Seat, i)
			}
		}
	}
}

// Each seat gets its own script, because the refund branch names only that
// member. Two seats sharing an address would mean one could spend the other's
// refund.
func TestEverySeatGetsItsOwnDepositAddress(t *testing.T) {
	_, deposits, err := Close(members(t, 6), testParams())
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	seen := make(map[string]uint32, len(deposits))
	for _, d := range deposits {
		if prev, dup := seen[d.DepositAddr]; dup {
			t.Fatalf("seats %d and %d share deposit address %s", prev, d.Seat, d.DepositAddr)
		}
		seen[d.DepositAddr] = d.Seat
	}
}

// Nothing is returned unless everything derived. The version this replaced
// wrote each script into its escrow as it went, so a failure part way left a
// table half committed to a membership that was never agreed.
func TestABadMemberYieldsNoDepositsAtAll(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members func(t *testing.T) []Member
	}{
		{"malformed key", func(t *testing.T) []Member {
			ms := members(t, 4)
			ms[2].CompPubkey = []byte{0x02, 0x03}
			return ms
		}},
		{"duplicate key", func(t *testing.T) []Member {
			ms := members(t, 4)
			ms[2].CompPubkey = ms[0].CompPubkey
			return ms
		}},
		{"zero csv", func(t *testing.T) []Member {
			ms := members(t, 4)
			ms[2].CSVBlocks = 0
			return ms
		}},
		{"over the member limit", func(t *testing.T) []Member {
			return members(t, escrow.MaxMembers+1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canonical, deposits, err := Close(tc.members(t), testParams())
			if err == nil {
				t.Fatalf("expected a refusal, got %d deposits", len(deposits))
			}
			if canonical != nil || deposits != nil {
				t.Fatalf("refused but still returned canonical=%v deposits=%v", canonical, deposits)
			}
		})
	}
}

func TestMatchIDAppendsASessionOnlyWhenThereIsOne(t *testing.T) {
	const tableID = "0123456789abcdef0123456789abcdef"
	if got := MatchID(tableID, ""); got != tableID {
		t.Fatalf("got %q, want the bare table id", got)
	}
	if got := MatchID("  "+tableID+"  ", "  "); got != tableID {
		t.Fatalf("got %q, want whitespace trimmed", got)
	}
	if got, want := MatchID(tableID, "sess1"), tableID+"|sess1"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
