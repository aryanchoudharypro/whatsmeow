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
	got, mappings := (&Client{}).parseBlocklist(node)
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
	if got.AddressingMode != types.AddressingModeLID || got.DHash != "1784533331843123" {
		t.Fatalf("list attributes: mode %q dhash %q", got.AddressingMode, got.DHash)
	}
	// Only the active LID-PN pair is learned; the inactive one would
	// overwrite the current mapping.
	if len(mappings) != 1 || mappings[0].LID != lid || mappings[0].PN != pn {
		t.Fatalf("LID mappings: %+v", mappings)
	}
	if lids := got.LIDs(); len(lids) != 3 || lids[0] != lid || lids[1] != oldLID || lids[2] != pn {
		t.Fatalf("LIDs(): %+v", lids)
	}
	if !got.IsBlocked(lid) || !got.IsBlocked(oldLID) || !got.IsBlocked(pn) {
		t.Fatal("IsBlocked missed a listed JID")
	}
	if got.IsBlocked(types.NewJID("919800000002", types.DefaultUserServer)) || got.IsBlocked(types.EmptyJID) {
		t.Fatal("IsBlocked matched an unlisted JID")
	}
}
