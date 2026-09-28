package whatsmeow

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestBuildEventCreation(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	if _, err := cli.BuildEventCreation(EventCreation{Name: "  "}); err == nil {
		t.Fatal("an unnamed event was built")
	}
	start := time.Unix(1_700_000_000, 0)
	msg, err := cli.BuildEventCreation(EventCreation{Name: "Launch", Description: "desc", StartTime: start, JoinLink: "https://call", ExtraGuestsAllowed: true})
	if err != nil {
		t.Fatal(err)
	}
	ev := msg.GetEventMessage()
	if ev.GetName() != "Launch" || ev.GetDescription() != "desc" || ev.GetStartTime() != start.Unix() ||
		ev.GetJoinLink() != "https://call" || !ev.GetExtraGuestsAllowed() || ev.EndTime != nil {
		t.Fatalf("event = %v", ev)
	}
	if len(msg.GetMessageContextInfo().GetMessageSecret()) != 32 {
		t.Fatal("event has no 32-byte message secret")
	}
}
