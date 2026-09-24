// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func TestParseNewsletterPollVotes(t *testing.T) {
	yes, no := sha256.Sum256([]byte("Yes")), sha256.Sum256([]byte("No"))
	cli := &Client{}
	node := &waBinary.Node{Tag: "messages", Content: []waBinary.Node{
		{Tag: "message", Attrs: waBinary.Attrs{"id": "A", "server_id": "10", "t": "1700000000", "type": "poll"},
			Content: []waBinary.Node{{Tag: "votes", Content: []waBinary.Node{
				{Tag: "vote", Attrs: waBinary.Attrs{"count": "7"}, Content: yes[:]},
				{Tag: "vote", Attrs: waBinary.Attrs{"count": "2"}, Content: no[:]},
				{Tag: "vote", Attrs: waBinary.Attrs{"count": "5"}, Content: []byte("short")},
			}}}},
	}}
	msgs := cli.parseNewsletterMessages(node)
	if len(msgs) != 1 || len(msgs[0].PollVotes) != 2 || msgs[0].PollVotes[yes] != 7 || msgs[0].PollVotes[no] != 2 {
		t.Fatalf("votes = %+v", msgs)
	}
}

func TestNewsletterPinnedMessages(t *testing.T) {
	var meta types.NewsletterThreadMetadata
	err := json.Unmarshal([]byte(`{"pinned_messages":[{"message_id":"125","expiry_ts":1700000000},{"message_id":126,"expiry_ts":"0"}]}`), &meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.PinnedMessages) != 2 {
		t.Fatalf("pins = %+v", meta.PinnedMessages)
	}
	first, second := meta.PinnedMessages[0], meta.PinnedMessages[1]
	if first.MessageID() != "125" || first.Expiry().Unix() != 1700000000 {
		t.Errorf("first pin = %s %v", first.MessageID(), first.Expiry())
	}
	if second.MessageID() != "126" || !second.Expiry().IsZero() {
		t.Errorf("second pin = %s %v", second.MessageID(), second.Expiry())
	}
}

func TestSimilarNewslettersParse(t *testing.T) {
	page, err := parseNewsletterDirectoryPage([]byte(`{"xwa2_newsletters_similar":{"result":[
		{"id":"120363000000000001@newsletter","thread_metadata":{"name":{"text":"News"},"verification":"VERIFIED"}}]}}`), "xwa2_newsletters_similar")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Channels) != 1 || page.Channels[0].Name != "News" || !page.Channels[0].Verified {
		t.Errorf("similar = %+v", page.Channels)
	}
}

func TestParseABProps(t *testing.T) {
	resp := &waBinary.Node{Tag: "iq", Content: []waBinary.Node{{Tag: "props", Attrs: waBinary.Attrs{"protocol": "1"}, Content: []waBinary.Node{
		{Tag: "prop", Attrs: waBinary.Attrs{"config_code": "29516", "config_value": "false"}},
		{Tag: "prop", Attrs: waBinary.Attrs{"config_code": "29517", "config_value": "1"}},
		{Tag: "prop", Attrs: waBinary.Attrs{"event_code": "5138", "sampling_weight": "-1"}},
	}}}}
	props := parseABProps(resp)
	if len(props) != 2 || props.Bool(ABPropChannelsPinAdminEnabled, true) || !props.Bool(ABPropChannelsPinFollowerEnabled, false) || !props.Bool(1, true) {
		t.Errorf("props = %v", props)
	}
}
