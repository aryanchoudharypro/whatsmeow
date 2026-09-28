package whatsmeow

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestResendLimiterBurstThenRefill(t *testing.T) {
	var l resendLimiter
	chat := types.NewJID("123", types.GroupServer)
	now := time.Unix(1000, 0)
	for i := range resendBurst {
		if !l.allow(chat, now) {
			t.Fatalf("resend %d throttled inside the burst", i)
		}
	}
	if l.allow(chat, now) {
		t.Fatal("burst exceeded")
	}
	if !l.allow(types.NewJID("456", types.GroupServer), now) {
		t.Fatal("another chat was throttled")
	}
	// 10 per minute: one token every 6 seconds.
	if l.allow(chat, now.Add(5*time.Second)) {
		t.Fatal("refilled too fast")
	}
	if !l.allow(chat, now.Add(7*time.Second)) {
		t.Fatal("didn't refill")
	}
	// An idle chat refills to the burst, not beyond.
	later := now.Add(time.Hour)
	allowed := 0
	for range resendBurst + 5 {
		if l.allow(chat, later) {
			allowed++
		}
	}
	if allowed != resendBurst {
		t.Fatalf("after idling, %d resends allowed, want %d", allowed, resendBurst)
	}
}
