// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package listener

import (
	"os"
	"strings"
	"testing"
)

// The bridge never reaches out to a game.
//
// Games connect in, and that direction is the claim: a game needs no inbound
// port, no forwarding and no route back to it, and the bridge keeps no list of
// addresses to try. A dialer here would quietly restore the arrangement where
// the bridge went looking for game processes it expected to find.
func TestTheBridgeNeverReachesOutToAGame(t *testing.T) {
	dialers := []string{"net.Dial", "grpc.NewClient", "grpc.Dial", "http.Get", "http.Post"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the listener package: %v", err)
	}
	for _, e := range entries {
		// The tests dial on purpose - they are standing in for the game.
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, d := range dialers {
			if strings.Contains(string(b), d) {
				t.Errorf("%s calls %s: the bridge is reaching out to something, so a game would "+
					"need a reachable address after all", e.Name(), d)
			}
		}
	}
}
