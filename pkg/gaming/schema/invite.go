package schema

import (
	"fmt"
	"github.com/karamble/dcrgaming-sdk/pkg/finance"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const InviteScheme = "gaming"
const InviteKindTable = "table"

var inviteGameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var inviteSIDRE = regexp.MustCompile(`^[0-9a-f]{1,32}$`)

type Invite struct {
	Game            string
	Kind            string
	BuyInAtoms      uint64
	Seats           uint32
	SID             string
	CSVBlocks       uint32
	Until           uint32
	AdmissionAtoms  uint64
	AdmissionBlocks uint32
	TableBondAtoms  uint64
	TableBondBlocks uint32
}

func (i Invite) Validate() error {
	if !inviteGameRE.MatchString(i.Game) || i.Kind != InviteKindTable || !inviteSIDRE.MatchString(i.SID) || i.Seats < 2 || i.Seats > finance.MaxMembers || i.BuyInAtoms == 0 || i.BuyInAtoms > uint64(finance.MaxAtoms)/uint64(i.Seats) || i.CSVBlocks == 0 || i.CSVBlocks > finance.MaxLockBlocks || i.Until == 0 || i.AdmissionAtoms == 0 || i.AdmissionAtoms > uint64(finance.MaxAtoms) || i.AdmissionBlocks == 0 || i.AdmissionBlocks > finance.MaxLockBlocks || i.TableBondAtoms > uint64(finance.MaxAtoms) || (i.TableBondAtoms == 0 && i.TableBondBlocks != 0) || (i.TableBondAtoms > 0 && (i.TableBondBlocks == 0 || i.TableBondBlocks > finance.MaxLockBlocks)) {
		return fmt.Errorf("invalid version 2 table financial terms")
	}
	return nil
}
func (i Invite) String() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	q := url.Values{"fv": {"2"}, "sid": {i.SID}}
	values := map[string]uint64{"buyin": i.BuyInAtoms, "seats": uint64(i.Seats), "csv": uint64(i.CSVBlocks), "until": uint64(i.Until), "bond": i.AdmissionAtoms, "bondcsv": uint64(i.AdmissionBlocks), "tablebond": i.TableBondAtoms, "tablebondcsv": uint64(i.TableBondBlocks)}
	for key, value := range values {
		q.Set(key, strconv.FormatUint(value, 10))
	}
	return (&url.URL{Scheme: InviteScheme, Host: i.Game, Path: "/" + i.Kind, RawQuery: q.Encode()}).String(), nil
}
func ParseInvite(link string) (Invite, error) {
	var empty Invite
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Scheme != InviteScheme || u.User != nil || u.Fragment != "" || u.Path != "/table" {
		return empty, fmt.Errorf("invalid gaming invitation")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return empty, err
	}
	for _, v := range q {
		if len(v) != 1 {
			return empty, fmt.Errorf("duplicate invitation parameter")
		}
	}
	if q.Get("fv") != "2" {
		return empty, fmt.Errorf("financial protocol version 2 required")
	}
	i := Invite{Game: u.Host, Kind: InviteKindTable, SID: q.Get("sid")}
	fields := map[string]*uint64{"buyin": &i.BuyInAtoms, "bond": &i.AdmissionAtoms, "tablebond": &i.TableBondAtoms}
	for key, dst := range fields {
		v, err := strconv.ParseUint(q.Get(key), 10, 64)
		if err != nil {
			return empty, fmt.Errorf("invalid invitation %s", key)
		}
		*dst = v
	}
	blocks := map[string]*uint32{"seats": &i.Seats, "csv": &i.CSVBlocks, "until": &i.Until, "bondcsv": &i.AdmissionBlocks, "tablebondcsv": &i.TableBondBlocks}
	for key, dst := range blocks {
		v, err := strconv.ParseUint(q.Get(key), 10, 32)
		if err != nil {
			return empty, fmt.Errorf("invalid invitation %s", key)
		}
		*dst = uint32(v)
	}
	if err = i.Validate(); err != nil {
		return empty, err
	}
	return i, nil
}
