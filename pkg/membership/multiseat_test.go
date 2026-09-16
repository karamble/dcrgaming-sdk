package membership

import (
	"fmt"
	"reflect"
	"testing"
)

func TestTwoFourSixPeersIndependentlyAgreeOnSeating(t *testing.T) {
	for _, n := range []int{2, 4, 6} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			peers := settleAll(t, testTerms(uint32(n)), players(t, n))
			for _, peer := range peers {
				if err := peer.SetBeacon(testBeacon()); err != nil {
					t.Fatal(err)
				}
			}
			expected, ok := peers[0].Seats()
			if !ok || len(expected) != n {
				t.Fatal("missing seating")
			}
			roster, _ := peers[0].RosterHash()
			for _, peer := range peers {
				got, ok := peer.Seats()
				hash, _ := peer.RosterHash()
				if !ok || !reflect.DeepEqual(got, expected) || hash != roster {
					t.Fatal("peers disagree")
				}
			}
		})
	}
}
