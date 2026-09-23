// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"testing"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestInviteCodeForms(t *testing.T) {
	for in, want := range map[string]string{
		"https://chat.whatsapp.com/AbC123":             "AbC123",
		"https://chat.whatsapp.com/invite/AbC123":      "AbC123",
		"https://chat.whatsapp.com/AbC123/?utm=x":      "AbC123",
		"whatsapp://chat/?code=AbC123":                 "AbC123",
		"https://web.whatsapp.com/accept/?code=AbC123": "AbC123",
		"AbC123":                               "AbC123",
		"  https://chat.whatsapp.com/AbC123  ": "AbC123",
	} {
		if got := inviteCode(in); got != want {
			t.Errorf("inviteCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMembershipApprovalModeState(t *testing.T) {
	on := waBinary.Node{Tag: "membership_approval_mode", Content: []waBinary.Node{{Tag: "group_join", Attrs: waBinary.Attrs{"state": "on"}}}}
	off := waBinary.Node{Tag: "membership_approval_mode", Content: []waBinary.Node{{Tag: "group_join", Attrs: waBinary.Attrs{"state": "off"}}}}
	bare := waBinary.Node{Tag: "membership_approval_mode"}
	if !membershipApprovalEnabled(&on) || membershipApprovalEnabled(&off) || !membershipApprovalEnabled(&bare) {
		t.Error("membership approval state misread")
	}
}

func TestParseGroupChangeCommunityAndIdentityDetails(t *testing.T) {
	group := types.NewJID("123", types.GroupServer)
	actorLID := types.NewJID("111", types.HiddenUserServer)
	actorPN := types.NewJID("919000000001", types.DefaultUserServer)
	hidden := types.NewJID("222", types.HiddenUserServer)
	added := types.NewJID("333", types.HiddenUserServer)
	addedPN := types.NewJID("919000000003", types.DefaultUserServer)
	changed := types.NewJID("444", types.HiddenUserServer)
	node := &waBinary.Node{
		Tag: "notification",
		Attrs: waBinary.Attrs{
			"from": group, "participant": actorLID, "participant_pn": actorPN, "t": "1700000000",
		},
		Content: []waBinary.Node{
			{Tag: "add", Content: []waBinary.Node{
				{Tag: "participant", Attrs: waBinary.Attrs{"jid": hidden, "display_name": "+91∙∙∙∙∙∙∙∙62"}},
				{Tag: "participant", Attrs: waBinary.Attrs{"jid": added, "phone_number": addedPN}},
			}},
			{Tag: "promote", Content: []waBinary.Node{{Tag: "participant", Attrs: waBinary.Attrs{"jid": added}}}},
			{Tag: "modify", Content: []waBinary.Node{{Tag: "participant", Attrs: waBinary.Attrs{"jid": changed}}}},
			{Tag: "linked_group_promote", Content: []waBinary.Node{{Tag: "participant", Attrs: waBinary.Attrs{"jid": hidden}}}},
			{Tag: "not_allow_non_admin_sub_group_creation"},
			{Tag: "member_add_mode", Content: []byte("admin_add")},
		},
	}
	cli := &Client{Log: waLog.Noop}
	evt, lidPairs, redacted, err := cli.parseGroupChange(node)
	if err != nil {
		t.Fatal(err)
	}
	if len(evt.Join) != 2 || len(evt.Promote) != 1 || len(evt.Modify) != 1 || len(evt.LinkedGroupPromote) != 1 {
		t.Errorf("participant lists wrong: %+v", evt)
	}
	if evt.AllowNonAdminSubGroupCreation == nil || *evt.AllowNonAdminSubGroupCreation {
		t.Error("expected non-admin subgroup creation to be turned off")
	}
	if evt.MemberAddMode == nil || *evt.MemberAddMode != types.GroupMemberAddModeAdmin {
		t.Error("expected member add mode admin_add")
	}
	// The actor's mapping and the added member's must both survive the later
	// participant lists.
	want := map[types.JID]types.JID{actorLID: actorPN, added: addedPN}
	for _, pair := range lidPairs {
		if want[pair.LID] == pair.PN {
			delete(want, pair.LID)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing LID mappings: %v (got %v)", want, lidPairs)
	}
	if len(redacted) != 1 || redacted[0].JID != hidden || redacted[0].RedactedPhone != "+91∙∙∙∙∙∙∙∙62" {
		t.Errorf("redacted phones = %+v", redacted)
	}
}

func TestGroupsDirtyDropsCachedGroups(t *testing.T) {
	stale := types.NewJID("1", types.GroupServer)
	kept := types.NewJID("2", types.GroupServer)
	cli := &Client{Log: waLog.Noop, groupCache: map[types.JID]*groupMetaCache{stale: {}, kept: {}}}
	node := &waBinary.Node{
		Tag:   "notification",
		Attrs: waBinary.Attrs{"from": types.ServerJID, "t": "1700000000"},
		Content: []waBinary.Node{{Tag: "groups_dirty", Content: []waBinary.Node{
			{Tag: "group", Attrs: waBinary.Attrs{"jid": stale}},
		}}},
	}
	evt, _, _, err := cli.parseGroupNotification(node)
	if err != nil {
		t.Fatal(err)
	}
	dirty, ok := evt.(*events.GroupsDirty)
	if !ok || len(dirty.Groups) != 1 || dirty.Groups[0] != stale {
		t.Fatalf("event = %#v", evt)
	}
	if _, ok := cli.groupCache[stale]; ok {
		t.Error("stale group still cached")
	}
	if _, ok := cli.groupCache[kept]; !ok {
		t.Error("unrelated group dropped from cache")
	}
}

func TestCommunityNodeIsParent(t *testing.T) {
	cli := &Client{Log: waLog.Noop}
	info, err := cli.parseGroupNode(&waBinary.Node{Tag: "community", Attrs: waBinary.Attrs{"id": "555", "creation": "1700000000"}})
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsParent || info.GroupCreated.Before(time.Unix(1, 0)) {
		t.Errorf("community node parsed as %+v", info)
	}
}
