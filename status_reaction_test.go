package whatsmeow

import (
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestStatusSettingAttr(t *testing.T) {
	text := &waE2E.Message{Conversation: new("hi")}
	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}}
	group := types.NewJID("123", types.GroupServer)
	for _, tc := range []struct {
		to      types.JID
		msg     *waE2E.Message
		setting string
		want    string
		wantErr bool
	}{
		{types.StatusBroadcastJID, text, StatusSettingAllowList, "allowlist", false},
		{types.StatusBroadcastJID, text, StatusSettingDenyList, "denylist", false},
		{types.StatusBroadcastJID, text, StatusSettingContacts, "contacts", false},
		{types.StatusBroadcastJID, text, "", "", false},
		{types.StatusBroadcastJID, revoke, StatusSettingContacts, "", false},
		{group, text, StatusSettingContacts, "", false},
		{types.StatusBroadcastJID, text, "everyone", "", true},
	} {
		got, err := statusSettingAttr(tc.to, tc.msg, tc.setting)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("statusSettingAttr(%s, %q) = %q, %v; want %q, err %v", tc.to, tc.setting, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestFinishStatusDirectNode(t *testing.T) {
	to := waBinary.Node{Tag: "to", Attrs: waBinary.Attrs{"jid": "1@lid"}}
	node := &waBinary.Node{Tag: "message", Content: []waBinary.Node{
		{Tag: "participants", Content: []waBinary.Node{to}},
		{Tag: "device-identity"},
		*statusAddOnMetaNode(),
	}}
	if !finishStatusDirectNode(node) {
		t.Fatal("reported no recipients")
	}
	var tags []string
	for _, child := range node.GetChildren() {
		tags = append(tags, child.Tag)
	}
	want := []string{"participants", "enc", "device-identity", "meta"}
	if len(tags) != len(want) {
		t.Fatalf("children %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Fatalf("children %v, want %v", tags, want)
		}
	}
	enc := node.GetChildren()[1]
	if enc.Attrs["type"] != "skmsg" || enc.Attrs["v"] != "2" || enc.Content != nil {
		t.Fatalf("bad empty skmsg: %v", enc)
	}
	if meta := node.GetChildren()[3]; meta.Attrs["content_type"] != "add_on" {
		t.Fatalf("bad meta: %v", meta)
	}

	empty := &waBinary.Node{Tag: "message", Content: []waBinary.Node{{Tag: "participants"}}}
	if finishStatusDirectNode(empty) {
		t.Fatal("empty participants accepted")
	}
}

func TestStatusReactionKey(t *testing.T) {
	cli := &Client{}
	poster := types.NewJID("999", types.HiddenUserServer)
	msg := cli.BuildReaction(types.StatusBroadcastJID, poster, "STATUSID", StatusLikeEmoji)
	key := msg.GetReactionMessage().GetKey()
	if key.GetRemoteJID() != "status@broadcast" || key.GetFromMe() || key.GetID() != "STATUSID" || key.GetParticipant() != "999@lid" {
		t.Fatalf("bad key: %v", key)
	}
	if msg.GetReactionMessage().GetText() != "\U0001F49A" {
		t.Fatal("bad like emoji")
	}
}
