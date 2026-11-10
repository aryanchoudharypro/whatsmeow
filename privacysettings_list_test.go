package whatsmeow

import (
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func TestParsePrivacyDisallowedList(t *testing.T) {
	lid := types.NewJID("123456789", types.HiddenUserServer)
	pn := types.NewJID("919876543210", types.DefaultUserServer)
	node := waBinary.Node{Tag: "privacy", Content: []waBinary.Node{{
		Tag:   "list",
		Attrs: waBinary.Attrs{"name": "last", "value": "contact_blacklist", "dhash": "1700000000000"},
		Content: []waBinary.Node{
			{Tag: "user", Attrs: waBinary.Attrs{"jid": lid, "pn_jid": pn}},
			{Tag: "user", Attrs: waBinary.Attrs{"jid": types.NewJID("555", types.HiddenUserServer)}},
		},
	}, {
		Tag:   "list",
		Attrs: waBinary.Attrs{"name": "profile", "dhash": "other"},
	}}}
	list := parsePrivacyDisallowedList(&node, types.PrivacySettingTypeLastSeen)
	if list.DHash != "1700000000000" {
		t.Fatalf("dhash = %q", list.DHash)
	}
	if len(list.Users) != 2 || list.Users[0].JID != lid || list.Users[0].PN != pn || !list.Users[1].PN.IsEmpty() {
		t.Fatalf("users = %+v", list.Users)
	}
}

func TestParsePrivacyDisallowedListShapes(t *testing.T) {
	lid := types.NewJID("123456789", types.HiddenUserServer)
	pn := types.NewJID("919876543210", types.DefaultUserServer)
	lidReply := waBinary.Node{Tag: "privacy", Attrs: waBinary.Attrs{"addressing_mode": "lid"}, Content: []waBinary.Node{{
		Tag:   "list",
		Attrs: waBinary.Attrs{"dhash": "h1"},
		Content: []waBinary.Node{
			{Tag: "user", Attrs: waBinary.Attrs{"pn_jid": pn}},
			{Tag: "user", Attrs: waBinary.Attrs{"username": "someone"}},
		},
	}}}
	list := parsePrivacyDisallowedList(&lidReply, types.PrivacySettingTypeLastSeen)
	if len(list.Users) != 1 || list.Users[0].JID != pn || list.Users[0].PN != pn {
		t.Fatalf("lid reply users = %+v", list.Users)
	}
	pnReply := waBinary.Node{Tag: "privacy", Content: []waBinary.Node{{
		Tag:   "list",
		Attrs: waBinary.Attrs{"dhash": "h2"},
		Content: []waBinary.Node{
			{Tag: "user", Attrs: waBinary.Attrs{"jid": pn, "lid": lid}},
		},
	}}}
	list = parsePrivacyDisallowedList(&pnReply, types.PrivacySettingTypeLastSeen)
	if len(list.Users) != 1 || list.Users[0].JID != lid || list.Users[0].PN != pn {
		t.Fatalf("pn reply users = %+v", list.Users)
	}
	empty := waBinary.Node{Tag: "privacy", Attrs: waBinary.Attrs{"addressing_mode": "lid"}}
	if list = parsePrivacyDisallowedList(&empty, types.PrivacySettingTypeLastSeen); len(list.Users) != 0 || list.DHash != "" {
		t.Fatalf("empty reply = %+v", list)
	}
}
