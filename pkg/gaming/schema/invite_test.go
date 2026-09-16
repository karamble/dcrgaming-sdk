package schema

import (
	"net/url"
	"strings"
	"testing"
)

func validInvite() Invite {
	return Invite{Game: "stakewars", Kind: InviteKindTable, SID: "abc123", Seats: 6, BuyInAtoms: 10000000, CSVBlocks: 288, Until: 900, AdmissionAtoms: 1000000, AdmissionBlocks: 2016}
}
func TestInviteRoundTripsExplicitEconomics(t *testing.T) {
	for _, bond := range []uint64{0, 2000000} {
		want := validInvite()
		want.TableBondAtoms = bond
		if bond != 0 {
			want.TableBondBlocks = 300
		}
		link, err := want.String()
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseInvite(link)
		if err != nil || got != want {
			t.Fatalf("roundtrip: %+v %v", got, err)
		}
	}
}
func TestInviteRejectsMissingOrDuplicateTerms(t *testing.T) {
	link, _ := validInvite().String()
	for _, field := range []string{"fv", "sid", "buyin", "seats", "csv", "until", "bond", "bondcsv", "tablebond", "tablebondcsv"} {
		t.Run(field, func(t *testing.T) {
			u, _ := url.Parse(link)
			q := u.Query()
			q.Del(field)
			u.RawQuery = q.Encode()
			if _, err := ParseInvite(u.String()); err == nil {
				t.Fatal("missing financial term accepted")
			}
			if _, err := ParseInvite(link + "&" + field + "=1"); err == nil {
				t.Fatal("duplicate term accepted")
			}
		})
	}
	if _, err := (Invite{Game: "stakewars", Kind: InviteKindTable}).String(); err == nil {
		t.Fatal("term-free invite accepted")
	}
}
func TestInviteRejectsInvalidEconomics(t *testing.T) {
	for name, change := range map[string]func(*Invite){
		"zero stake":                  func(i *Invite) { i.BuyInAtoms = 0 },
		"pot overflow":                func(i *Invite) { i.BuyInAtoms = 21000000 * 100000000 },
		"zero bond":                   func(i *Invite) { i.AdmissionAtoms = 0 },
		"bond delay overflow":         func(i *Invite) { i.AdmissionBlocks = 65536 },
		"stake delay overflow":        func(i *Invite) { i.CSVBlocks = 65536 },
		"missing optional bond delay": func(i *Invite) { i.TableBondAtoms = 1 },
		"delay without optional bond": func(i *Invite) { i.TableBondBlocks = 1 },
		"no deadline":                 func(i *Invite) { i.Until = 0 },
		"too many seats":              func(i *Invite) { i.Seats = 14 },
		"too few seats":               func(i *Invite) { i.Seats = 1 },
		"invalid session":             func(i *Invite) { i.SID = "NOTHEX" },
	} {
		t.Run(name, func(t *testing.T) {
			i := validInvite()
			change(&i)
			if _, err := i.String(); err == nil {
				t.Fatal("invalid terms accepted")
			}
		})
	}
	link, _ := validInvite().String()
	for _, malformed := range []string{strings.Replace(link, "fv=2", "fv=1", 1), strings.Replace(link, "gaming://", "https://", 1), link + "#fragment", strings.Replace(link, "csv=288", "csv=soon", 1)} {
		if _, err := ParseInvite(malformed); err == nil {
			t.Fatalf("malformed invite accepted: %s", malformed)
		}
	}
}
