// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func TestParseNewsletterFollowers(t *testing.T) {
	data := []byte(`{"xwa2_newsletter_followers":{"followers":{"edges":[
		{"role":"ADMIN","follow_time":"1700000000","node":{"id":"111111111111111@lid","pn":"12025550111@s.whatsapp.net","display_name":"Admin","username_info":{"username":"the.admin"}}},
		{"role":"SUBSCRIBER","follow_time":1700000001,"node":{"id":"222222222222222@lid","display_name":"Follower"}},
		{"role":"SUBSCRIBER","node":{}}]}}}`)
	followers, err := parseNewsletterFollowers(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(followers) != 2 {
		t.Fatalf("got %d followers, want 2 (the one with no id is dropped)", len(followers))
	}
	admin, plain := followers[0], followers[1]
	if admin.Role != types.NewsletterRoleAdmin || admin.PhoneJID.User != "12025550111" || admin.Username != "the.admin" || admin.FollowTime != 1700000000 {
		t.Errorf("admin = %+v", admin)
	}
	if plain.Role != types.NewsletterRoleSubscriber || !plain.PhoneJID.IsEmpty() || plain.JID.Server != types.HiddenUserServer {
		t.Errorf("follower = %+v", plain)
	}
	if _, err = parseNewsletterFollowers([]byte(`{"xwa2_newsletter_followers":null}`)); err == nil {
		t.Error("a null follower list should be an error")
	}
}

func TestParseNewsletterMessagesSenderAndEdits(t *testing.T) {
	cli := &Client{}
	node := &waBinary.Node{Tag: "messages", Content: []waBinary.Node{
		{Tag: "message", Attrs: waBinary.Attrs{"id": "A", "server_id": "10", "t": "1700000000", "type": "text", "is_sender": "true"},
			Content: []waBinary.Node{{Tag: "plaintext", Content: []byte{0x0a, 0x02, 'h', 'i'}}}},
		{Tag: "message", Attrs: waBinary.Attrs{"id": "B", "server_id": "11", "t": "1700000001", "type": "text", "edit": "8"},
			Content: []waBinary.Node{{Tag: "plaintext", Content: []byte{}}}},
	}}
	msgs := cli.parseNewsletterMessages(node)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if !msgs[0].IsSender || msgs[0].Message.GetConversation() != "hi" || msgs[0].Edit != types.EditAttributeEmpty {
		t.Errorf("own post = %+v", msgs[0])
	}
	if msgs[1].IsSender || msgs[1].Message != nil || msgs[1].Edit != types.EditAttributeAdminRevoke {
		t.Errorf("deleted post = %+v", msgs[1])
	}
}
