package whatsmeow

import (
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func TestParseBlocklistKeepsPhoneNumberAndActive(t *testing.T) {
	lid := types.NewJID("108701327343799", types.HiddenUserServer)
	oldLID := types.NewJID("1099645911040", types.HiddenUserServer)
	pn := types.NewJID("919800000001", types.DefaultUserServer)
	node := &waBinary.Node{
		Tag:   "list",
		Attrs: waBinary.Attrs{"addressing_mode": "lid", "dhash": "1784533331843123"},
		Content: []waBinary.Node{
			{Tag: "item", Attrs: waBinary.Attrs{"jid": lid, "pn_jid": pn, "active": "true"}},
			{Tag: "item", Attrs: waBinary.Attrs{"jid": oldLID, "pn_jid": pn}},
			{Tag: "item", Attrs: waBinary.Attrs{"jid": pn}},
		},
	}
	got := (&Client{}).parseBlocklist(node)
	if len(got.JIDs) != 3 || len(got.Entries) != 3 {
		t.Fatalf("want 3 JIDs and entries, got %+v", got)
	}
	if e := got.Entries[0]; e.JID != lid || e.PN != pn || !e.Active {
		t.Fatalf("active entry: %+v", e)
	}
	if e := got.Entries[1]; e.JID != oldLID || e.PN != pn || e.Active {
		t.Fatalf("inactive entry: %+v", e)
	}
	if e := got.Entries[2]; e.JID != pn || !e.PN.IsEmpty() || e.Active {
		t.Fatalf("entry without a phone number attribute: %+v", e)
	}
}
