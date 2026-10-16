// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"bytes"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types/events"
)

func fakeHistorySync() *waHistorySync.HistorySync {
	hs := &waHistorySync.HistorySync{
		SyncType:   waHistorySync.HistorySync_FULL.Enum(),
		ChunkOrder: proto.Uint32(3),
		Progress:   proto.Uint32(40),
		Pushnames:  []*waHistorySync.Pushname{{ID: proto.String("15550000009@s.whatsapp.net"), Pushname: proto.String("Test")}},
		GlobalSettings: &waHistorySync.GlobalSettings{
			ChatDbLidMigrationTimestamp: proto.Int64(1700000000),
		},
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{{PnJID: proto.String("15550000009@s.whatsapp.net"), LidJID: proto.String("100000000000009@lid")}},
	}
	for c := range 5 {
		conv := &waHistorySync.Conversation{ID: proto.String(fmt.Sprintf("1555000%04d@s.whatsapp.net", c))}
		if c == 2 {
			conv.ID = proto.String("120363000000000001@g.us")
		}
		if c%2 == 0 {
			conv.TcToken = []byte{byte(c), 1, 2}
			conv.TcTokenTimestamp = proto.Uint64(1700000000 + uint64(c))
			conv.TcTokenSenderTimestamp = proto.Uint64(1700000100 + uint64(c))
		}
		for m := range 30 {
			wmi := &waWeb.WebMessageInfo{
				Key: &waCommon.MessageKey{
					RemoteJID: conv.ID,
					FromMe:    proto.Bool(m%3 == 0),
					ID:        proto.String(fmt.Sprintf("3EB0%02d%02d", c, m)),
				},
				Message:          &waE2E.Message{Conversation: proto.String(fmt.Sprintf("hello %d %d", c, m))},
				MessageTimestamp: proto.Uint64(1700000000 + uint64(m)),
			}
			if c == 2 {
				wmi.Key.Participant = proto.String(fmt.Sprintf("1555111%04d@s.whatsapp.net", m))
			}
			if m%4 == 0 {
				wmi.MessageSecret = bytes.Repeat([]byte{byte(m)}, 32)
			}
			conv.Messages = append(conv.Messages, &waHistorySync.HistorySyncMsg{Message: wmi, MsgOrderID: proto.Uint64(uint64(m))})
		}
		hs.Conversations = append(hs.Conversations, conv)
	}
	return hs
}

func TestSplitHistorySyncMatchesFullDecode(t *testing.T) {
	orig := fakeHistorySync()
	raw, err := proto.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, 0x98, 0x06, 0x01) // unknown field 99 must survive
	rest, convs, err := splitHistorySync(raw)
	if err != nil {
		t.Fatal(err)
	}
	var full waHistorySync.HistorySync
	if err = proto.Unmarshal(raw, &full); err != nil {
		t.Fatal(err)
	}
	wantConvs := full.Conversations
	full.Conversations = nil
	if !proto.Equal(rest, &full) {
		t.Fatalf("non-conversation fields differ:\n%v\n%v", rest, &full)
	}
	if len(rest.ProtoReflect().GetUnknown()) != 3 {
		t.Fatal("unknown field lost")
	}
	if countHistoryMessages(convs) != 150 {
		t.Fatal("message count", countHistoryMessages(convs))
	}
	lazy := events.NewHistorySyncConversations(convs)
	if lazy.Len() != len(wantConvs) {
		t.Fatal("len", lazy.Len())
	}
	i := 0
	for conv, err := range lazy.All() {
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(conv, wantConvs[i]) {
			t.Fatalf("conversation %d differs", i)
		}
		i++
	}
	if i != len(wantConvs) {
		t.Fatal("yielded", i)
	}
}

func TestScanHistoryConversationFindsSecrets(t *testing.T) {
	orig := fakeHistorySync()
	for _, conv := range orig.Conversations {
		raw, _ := proto.Marshal(conv)
		scan, err := scanHistoryConversation(raw)
		if err != nil {
			t.Fatal(err)
		}
		if scan.id != conv.GetID() || !bytes.Equal(scan.tcToken, conv.GetTcToken()) ||
			scan.tcTokenTS != conv.GetTcTokenTimestamp() || scan.tcSenderTS != conv.GetTcTokenSenderTimestamp() ||
			scan.messages != len(conv.GetMessages()) {
			t.Fatalf("conversation header mismatch for %s: %+v", conv.GetID(), scan)
		}
		var want []historyMessageSecretRef
		for _, msg := range conv.GetMessages() {
			wmi := msg.GetMessage()
			if wmi.GetMessageSecret() == nil {
				continue
			}
			want = append(want, historyMessageSecretRef{
				id: wmi.GetKey().GetID(), fromMe: wmi.GetKey().GetFromMe(), keyPart: wmi.GetKey().GetParticipant(),
				participant: wmi.GetParticipant(), secret: wmi.GetMessageSecret(),
			})
		}
		if len(want) != len(scan.secrets) {
			t.Fatalf("%s: %d secrets, want %d", conv.GetID(), len(scan.secrets), len(want))
		}
		for i := range want {
			got := scan.secrets[i]
			if got.id != want[i].id || got.fromMe != want[i].fromMe || got.keyPart != want[i].keyPart ||
				got.participant != want[i].participant || !bytes.Equal(got.secret, want[i].secret) {
				t.Fatalf("secret %d: %+v, want %+v", i, got, want[i])
			}
		}
	}
	if _, err := scanHistoryConversation([]byte{0x0a, 0x7f}); err == nil {
		t.Fatal("truncated conversation accepted")
	}
	if _, _, err := splitHistorySync([]byte{0x12, 0x7f, 0x01}); err == nil {
		t.Fatal("truncated blob accepted")
	}
}
