// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"testing"
	"time"
)

func TestAutoReconnectBackoffFollowsWAWeb(t *testing.T) {
	want := []int{0, 1, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233, 377, 610, 900, 900}
	for n, secs := range want {
		base := time.Duration(secs) * time.Second
		for range 20 {
			got := autoReconnectBackoff(n)
			if got < base || got > base+base/10 {
				t.Fatalf("attempt %d: got %v, want %v plus at most 10%%", n, got, base)
			}
		}
	}
	if got := autoReconnectBackoff(1000); got > autoReconnectMaxDelay+autoReconnectMaxDelay/10 {
		t.Fatalf("attempt 1000: got %v, above the cap", got)
	}
}
