package appstate

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestMessageKeyParticipant(t *testing.T) {
	pn := types.NewJID("12025550111", types.DefaultUserServer)
	lid := types.NewJID("100000000001", types.HiddenUserServer)
	group := types.NewJID("120000000001", types.GroupServer)
	device := types.JID{User: "100000000001", Device: 7, Server: types.HiddenUserServer}
	cases := []struct {
		chat, sender types.JID
		fromMe       bool
		want         string
	}{
		{pn, pn, false, "0"},
		{lid, pn, false, "0"},
		{lid, types.EmptyJID, true, "0"},
		{group, pn, false, pn.String()},
		{group, device, false, lid.String()},
		{group, lid, true, "0"},
		{group, types.EmptyJID, true, "0"},
		{types.StatusBroadcastJID, pn, false, pn.String()},
	}
	for _, c := range cases {
		if got := messageKeyParticipant(c.chat, c.sender, c.fromMe); got != c.want {
			t.Errorf("messageKeyParticipant(%s, %s, %v) = %q, want %q", c.chat, c.sender, c.fromMe, got, c.want)
		}
	}
	if idx := BuildStar(group, types.EmptyJID, "ID", true, true).Mutations[0].Index; idx[4] != "0" {
		t.Errorf("own star in group has participant %q", idx[4])
	}
	if m := BuildDeleteMessageForMe(lid, pn, "ID", false, true, time.Time{}).Mutations[0]; m.Index[4] != "0" || m.Version != 3 {
		t.Errorf("DM delete for me = %v v%d", m.Index, m.Version)
	}
}
