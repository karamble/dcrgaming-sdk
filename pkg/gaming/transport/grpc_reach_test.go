// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package transport

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// hostErr must keep the gRPC code reachable: Unreachable is a money-safety
// predicate and was inert while hostErr rendered the status with %s.
func TestUnreachableSurvivesHostErr(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		want bool
	}{
		{codes.Unavailable, true},
		{codes.DeadlineExceeded, true},
		{codes.Unimplemented, true},
		{codes.ResourceExhausted, false},
		{codes.FailedPrecondition, false},
	} {
		raw := status.Error(tc.code, "x")
		if got := Unreachable(hostErr("ask about a payment", raw)); got != tc.want {
			t.Errorf("Unreachable(hostErr(%v)) = %v, want %v", tc.code, got, tc.want)
		}
	}
	// A non-status error still passes through unchanged and reads as reachable.
	if Unreachable(hostErr("x", errPlain)) {
		t.Error("a plain error should not read as unreachable")
	}
}

var errPlain = plainErr("plain")

type plainErr string

func (e plainErr) Error() string { return string(e) }
