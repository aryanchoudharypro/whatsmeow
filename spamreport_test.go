package whatsmeow

import (
	"testing"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestSpamListNodeShape(t *testing.T) {
	node := buildSpamListNode(SpamReport{
		MessageID: "TEST789", MessageTimestamp: 1234567890,
		Group: types.NewJID("120363025918861132", types.GroupServer), GroupSubject: "Test Group",
		Participant: types.NewJID("5511999887766", types.DefaultUserServer),
		Flow:        SpamFlowGroupInfoReport, RawMessage: []byte{1, 2, 3}, MediaType: "image",
	})
	ag := node.AttrGetter()
	if node.Tag != "spam_list" || ag.String("spam_flow") != "GroupInfoReport" || ag.String("subject") != "Test Group" ||
		ag.JID("jid").String() != "120363025918861132@g.us" {
		t.Fatalf("spam_list = %v", node)
	}
	message := node.GetChildByTag("message")
	if message.AttrGetter().String("id") != "TEST789" || message.AttrGetter().String("t") != "1234567890" {
		t.Fatalf("message = %v", message)
	}
	raw := message.GetChildByTag("raw")
	if raw.AttrGetter().String("v") != "3" || raw.AttrGetter().String("mediatype") != "image" {
		t.Fatalf("raw = %v", raw)
	}
	if plain := buildSpamListNode(SpamReport{MessageID: "X"}); plain.AttrGetter().String("spam_flow") != "MessageMenu" {
		t.Fatal("default flow isn't MessageMenu")
	}
}

func TestPresenceSubscriptionsAreRemembered(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	user := types.NewADJID("111", 0, 3)
	cli.trackPresenceSubscription(user, true)
	if !cli.isPresenceSubscriptionTracked(user.ToNonAD()) {
		t.Fatal("subscription not remembered under the bare JID")
	}
	cli.trackPresenceSubscription(user.ToNonAD(), false)
	if cli.isPresenceSubscriptionTracked(user) {
		t.Fatal("unsubscribe didn't forget it")
	}
}
