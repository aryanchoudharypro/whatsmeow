package whatsmeow

import (
	"testing"
	"time"
)

// A key request is answered by another of this account's devices, so silence
// usually means they were asleep when we asked. The first retries come quickly;
// only once the peer is plainly not merely asleep does the pace fall back to the
// passive floor.
func TestAppStateKeyRetryBackoffEscalatesThenSettles(t *testing.T) {
	if got := appStateKeyRetryDelay(0); got != 0 {
		t.Errorf("a key never asked for must be asked immediately, got %v", got)
	}
	var prev time.Duration
	for attempts := 1; attempts <= len(appStateKeyRetryBackoff); attempts++ {
		got := appStateKeyRetryDelay(attempts)
		if got <= prev {
			t.Errorf("delay after %d attempts = %v, must exceed the previous %v", attempts, got, prev)
		}
		if got >= appStateKeyRetryFloor {
			t.Errorf("delay after %d attempts = %v, must stay under the %v floor", attempts, got, appStateKeyRetryFloor)
		}
		prev = got
	}
	for _, attempts := range []int{len(appStateKeyRetryBackoff) + 1, 50} {
		if got := appStateKeyRetryDelay(attempts); got != appStateKeyRetryFloor {
			t.Errorf("delay after %d attempts = %v, want the %v floor", attempts, got, appStateKeyRetryFloor)
		}
	}
	// The first retry has to be soon enough to rescue a sync that is blocked
	// right now, which a day-long wait never did.
	if appStateKeyRetryDelay(1) > time.Minute {
		t.Errorf("first retry after %v is too slow to unblock a waiting sync", appStateKeyRetryDelay(1))
	}
}
