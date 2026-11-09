// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestScheduledMessageCryptoRoundTrip(t *testing.T) {
	inner := &waE2E.Message{Conversation: proto.String("see you at nine")}
	at := time.Unix(1794281400, 0)
	prepared, err := PrepareScheduledMessage(inner, at)
	if err != nil {
		t.Fatal(err)
	}
	if inner.MessageContextInfo != nil {
		t.Fatal("PrepareScheduledMessage changed the caller's message")
	}
	crm := prepared.Wrapped.GetConditionalRevealMessage()
	if crm.GetConditionalRevealMessageType() != waE2E.ConditionalRevealMessage_SCHEDULED_MESSAGE {
		t.Fatalf("type = %v", crm.GetConditionalRevealMessageType())
	}
	if len(crm.GetEncIV()) != 12 || len(prepared.RevealKey) != 32 {
		t.Fatalf("iv %d bytes, key %d bytes", len(crm.GetEncIV()), len(prepared.RevealKey))
	}
	if crm.GetRevealKeyID() != prepared.RevealKeyID || len(prepared.RevealKeyID) != 36 {
		t.Fatalf("reveal key ID %q / %q", crm.GetRevealKeyID(), prepared.RevealKeyID)
	}
	// AES-GCM appends a 16-byte tag.
	if len(crm.GetEncPayload()) != len(prepared.InnerPlaintext)+16 {
		t.Fatalf("payload %d bytes for %d bytes of plaintext", len(crm.GetEncPayload()), len(prepared.InnerPlaintext))
	}
	secret := prepared.Wrapped.GetMessageContextInfo().GetMessageSecret()
	if len(secret) != 32 || !bytes.Equal(secret, prepared.Inner.GetMessageContextInfo().GetMessageSecret()) {
		t.Fatal("wrapper and inner message don't share the message secret")
	}
	if !prepared.Time.Equal(at) {
		t.Fatalf("time = %v", prepared.Time)
	}

	got, err := DecryptScheduledMessage(crm, prepared.RevealKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetConversation() != "see you at nine" {
		t.Fatalf("decrypted %v", got)
	}
	if text, ok := ExtractScheduledMessageText(got); !ok || text != "see you at nine" {
		t.Fatalf("text = %q, %v", text, ok)
	}

	wrongKey := bytes.Repeat([]byte{1}, 32)
	if _, err = DecryptScheduledMessage(crm, wrongKey); err == nil {
		t.Fatal("decrypting with the wrong key worked")
	}
	if _, err = DecryptScheduledMessage(crm, prepared.RevealKey[:16]); !errors.Is(err, ErrInvalidRevealKey) {
		t.Fatalf("short key: %v", err)
	}
}

func TestDecryptScheduledMessageUnwrapsAssociatedChild(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	secret := bytes.Repeat([]byte{9}, 32)
	outer := &waE2E.Message{
		AssociatedChildMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{Caption: proto.String("photo")},
		}},
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: secret},
	}
	plaintext, _ := proto.Marshal(outer)
	iv, payload, err := EncryptWithRevealKey(plaintext, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecryptScheduledMessage(&waE2E.ConditionalRevealMessage{
		EncIV: iv, EncPayload: payload, RevealKeyID: proto.String("x"),
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetImageMessage().GetCaption() != "photo" || !bytes.Equal(got.GetMessageContextInfo().GetMessageSecret(), secret) {
		t.Fatalf("got %v", got)
	}
}

// The stanza a linked device got for a message scheduled on the phone:
// <message from=<own lid> recipient=<own lid> type=text ...>
// <enc decrypt-fail=hide type=msg v=2/>
// <meta st="1794281400" type="scheduled_message"><key rkid="84EE...">32 bytes</key></meta></message>
func observedScheduledStanza(revealKey []byte) waBinary.Node {
	ownLID := types.NewJID("100000000000001", types.HiddenUserServer)
	return waBinary.Node{
		Tag: "message",
		Attrs: waBinary.Attrs{
			"from":              ownLID,
			"id":                "3EB0AAAAAAAAAAAAAAAA",
			"notify":            "Tester",
			"peer_recipient_pn": types.NewJID("15550001111", types.DefaultUserServer),
			"recipient":         ownLID,
			"sts":               "1794200000000000",
			"t":                 "1794200000",
			"type":              "text",
		},
		Content: []waBinary.Node{
			{Tag: "enc", Attrs: waBinary.Attrs{"decrypt-fail": "hide", "type": "msg", "v": "2"}, Content: []byte{1, 2, 3}},
			{
				Tag:   "meta",
				Attrs: waBinary.Attrs{"st": "1794281400", "type": "scheduled_message"},
				Content: []waBinary.Node{{
					Tag:     "key",
					Attrs:   waBinary.Attrs{"rkid": "84EE4E55842799647766A38DB8AF4F8E"},
					Content: revealKey,
				}},
			},
		},
	}
}

func scheduledTestClient() *Client {
	pn := types.NewJID("15550001111", types.DefaultUserServer)
	pn.Device = 2
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	lid.Device = 2
	return &Client{Log: waLog.Noop, Store: &store.Device{ID: &pn, LID: lid}}
}

func TestParseObservedScheduledMessageStanza(t *testing.T) {
	revealKey := bytes.Repeat([]byte{0xAB}, 32)
	node := observedScheduledStanza(revealKey)
	// Through the binary encoding, as it comes off the wire.
	data, err := waBinary.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := waBinary.Unmarshal(data[1:]) // Marshal leaves the frame's flag byte in front
	if err != nil {
		t.Fatal(err)
	}
	cli := scheduledTestClient()
	info, err := cli.parseMessageInfo(decoded)
	if err != nil {
		t.Fatal(err)
	}
	sm := info.MsgMetaInfo.ScheduledMessage
	if sm == nil {
		t.Fatal("no scheduled message meta")
	}
	if sm.Time.Unix() != 1794281400 || sm.RevealKeyID != "84EE4E55842799647766A38DB8AF4F8E" || !bytes.Equal(sm.RevealKey, revealKey) {
		t.Fatalf("got %+v", sm)
	}
	if !info.IsFromMe || info.Chat.User != "100000000000001" {
		t.Fatalf("source %+v", info.MessageSource)
	}
}

func TestParseScheduledMetaVariants(t *testing.T) {
	key := bytes.Repeat([]byte{0x5C}, 32)
	hexKey := []byte(strings.ToUpper(hex.EncodeToString(key)))
	meta := func(attrs waBinary.Attrs, keyContent any) waBinary.Node {
		return waBinary.Node{Tag: "meta", Attrs: attrs, Content: []waBinary.Node{{
			Tag: "key", Attrs: waBinary.Attrs{"rkid": "abc"}, Content: keyContent,
		}}}
	}
	got, err := parseScheduledMessageMeta(meta(waBinary.Attrs{"type": "scheduled_message", "st": "100"}, hexKey))
	if err != nil || !bytes.Equal(got.RevealKey, key) {
		t.Fatalf("hex key: %v %+v", err, got)
	}
	got, err = parseScheduledMessageMeta(meta(waBinary.Attrs{"type": "scheduled_message"}, nil))
	if err != nil || got.RevealKey != nil || !got.Time.IsZero() || got.RevealKeyID != "abc" {
		t.Fatalf("unschedule meta: %v %+v", err, got)
	}
	if _, err = parseScheduledMessageMeta(meta(waBinary.Attrs{"type": "scheduled_message", "st": "100"}, []byte("short"))); err == nil {
		t.Fatal("a 5-byte key parsed")
	}
	if _, err = parseScheduledMessageMeta(waBinary.Node{Tag: "meta", Attrs: waBinary.Attrs{"type": "scheduled_message"}}); err == nil {
		t.Fatal("meta without key parsed")
	}
}

func TestScheduledMetaNodeMatchesObservedStanza(t *testing.T) {
	revealKey := bytes.Repeat([]byte{0xAB}, 32)
	sm := &scheduledSendMeta{
		time:        time.Unix(1794281400, 0),
		revealKeyID: "84EE4E55842799647766A38DB8AF4F8E",
		revealKey:   revealKey,
	}
	meta := waBinary.Node{Tag: "meta", Attrs: waBinary.Attrs{}}
	sm.addTo(&meta)
	stanza := observedScheduledStanza(revealKey)
	want := stanza.GetChildByTag("meta")
	if meta.String() != want.String() {
		t.Fatalf("built\n%s\nwant\n%s", meta.String(), want.String())
	}
	parsed, err := parseScheduledMessageMeta(meta)
	if err != nil || parsed.RevealKeyID != sm.revealKeyID || !bytes.Equal(parsed.RevealKey, revealKey) || !parsed.Time.Equal(sm.time) {
		t.Fatalf("round trip: %v %+v", err, parsed)
	}

	unschedule := &scheduledSendMeta{unschedule: true, revealKeyID: "84EE4E55842799647766A38DB8AF4F8E"}
	meta = waBinary.Node{Tag: "meta", Attrs: waBinary.Attrs{}}
	unschedule.addTo(&meta)
	if got := meta.String(); got != `<meta type="scheduled_message"><key rkid="84EE4E55842799647766A38DB8AF4F8E"/></meta>` {
		t.Fatalf("unschedule meta %s", got)
	}
}

func TestScheduledStanzaAttributes(t *testing.T) {
	prepared, err := PrepareScheduledMessage(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("x")}}, time.Unix(1794281400, 0))
	if err != nil {
		t.Fatal(err)
	}
	params := nodeExtraParams{msgType: getTypeFromMessage(prepared.Inner), mediaType: getMediaTypeFromMessage(prepared.Inner)}
	if params.typeOf(prepared.Wrapped) != "media" || params.mediaTypeOf(prepared.Wrapped) != "image" {
		t.Fatalf("type %q mediatype %q", params.typeOf(prepared.Wrapped), params.mediaTypeOf(prepared.Wrapped))
	}
	var plain nodeExtraParams
	if plain.typeOf(prepared.Wrapped) != "text" || plain.mediaTypeOf(prepared.Wrapped) != "" {
		t.Fatal("wrapper without overrides isn't plain text")
	}

	cli := scheduledTestClient()
	chat := types.NewJID("100000000000002", types.HiddenUserServer)
	unschedule := cli.BuildUnschedule(chat, "3EB0AAAAAAAAAAAAAAAA")
	if getEditAttribute(unschedule) != types.EditAttributeSenderRevoke {
		t.Fatalf("unschedule edit attribute %q", getEditAttribute(unschedule))
	}
	key := unschedule.GetProtocolMessage().GetKey()
	if !key.GetFromMe() || key.GetID() != "3EB0AAAAAAAAAAAAAAAA" || key.GetRemoteJID() != chat.String() {
		t.Fatalf("key %v", key)
	}
}

func TestCheckSchedulable(t *testing.T) {
	user := types.NewJID("100000000000002", types.HiddenUserServer)
	cases := []struct {
		to  types.JID
		msg *waE2E.Message
		ok  bool
	}{
		{user, &waE2E.Message{Conversation: proto.String("hi")}, true},
		{types.NewJID("1203630000000000", types.GroupServer), &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("hi")}}, true},
		{user, &waE2E.Message{VideoMessage: &waE2E.VideoMessage{}}, true},
		{user, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{ViewOnce: proto.Bool(true)}}, false},
		{user, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{}}, false},
		{types.NewJID("1203630000000000", types.NewsletterServer), &waE2E.Message{Conversation: proto.String("hi")}, false},
		{types.StatusBroadcastJID, &waE2E.Message{Conversation: proto.String("hi")}, false},
	}
	for i, c := range cases {
		err := checkSchedulable(c.to, c.msg)
		if (err == nil) != c.ok {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestIsScheduledTimeValid(t *testing.T) {
	now := time.Unix(1_800_000_030, 0) // 30 seconds into a minute
	minW, maxW := DefaultScheduledMessageMinWindow, DefaultScheduledMessageMaxWindow
	minuteStart := time.Unix(1_800_000_000, 0)
	cases := []struct {
		at time.Time
		ok bool
	}{
		{minuteStart.Add(10 * time.Minute), true},
		{minuteStart.Add(10*time.Minute - time.Second), false},
		{now.Add(14 * 24 * time.Hour), true},
		{now.Add(14*24*time.Hour + time.Second), false},
		{now.Add(-time.Hour), false},
	}
	for i, c := range cases {
		if got := IsScheduledTimeValid(c.at, now, minW, maxW); got != c.ok {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

func TestDefaultScheduledMessageTime(t *testing.T) {
	loc := time.FixedZone("test", 2*3600)
	minW, maxW := DefaultScheduledMessageMinWindow, DefaultScheduledMessageMaxWindow
	now := time.Date(2026, 11, 9, 10, 15, 30, 0, loc)
	if got := DefaultScheduledMessageTime(now, minW, maxW, false); !got.Equal(time.Date(2026, 11, 9, 10, 25, 0, 0, loc)) {
		t.Errorf("plain default %v", got)
	}
	if got := DefaultScheduledMessageTime(now, minW, maxW, true); !got.Equal(time.Date(2026, 11, 9, 17, 0, 0, 0, loc)) {
		t.Errorf("smart default before 16:30 %v", got)
	}
	early := time.Date(2026, 11, 9, 7, 0, 0, 0, loc)
	if got := DefaultScheduledMessageTime(early, minW, maxW, true); !got.Equal(time.Date(2026, 11, 9, 9, 0, 0, 0, loc)) {
		t.Errorf("smart default before 8:30 %v", got)
	}
	late := time.Date(2026, 11, 9, 20, 0, 0, 0, loc)
	if got := DefaultScheduledMessageTime(late, minW, maxW, true); !got.Equal(time.Date(2026, 11, 10, 9, 0, 0, 0, loc)) {
		t.Errorf("smart default evening %v", got)
	}
	// Clamped to the window on both sides.
	edge := time.Date(2026, 11, 9, 16, 25, 0, 0, loc)
	if got := DefaultScheduledMessageTime(edge, 45*time.Minute, maxW, true); !got.Equal(time.Date(2026, 11, 9, 17, 10, 0, 0, loc)) {
		t.Errorf("smart default clamped to min %v", got)
	}
	if got := DefaultScheduledMessageTime(late, minW, time.Hour, true); !got.Equal(time.Date(2026, 11, 9, 21, 0, 0, 0, loc)) {
		t.Errorf("smart default clamped to max %v", got)
	}
}

func TestScheduledMessagesConfig(t *testing.T) {
	cfg := scheduledMessagesConfigFromProps(nil)
	if cfg.Known || cfg.SenderEnabled || cfg.MinWindow != 10*time.Minute || cfg.MaxWindow != 14*24*time.Hour {
		t.Fatalf("defaults %+v", cfg)
	}
	cfg = scheduledMessagesConfigFromProps(ABProps{
		ABPropScheduledMessagesSenderEnabled:    "1",
		ABPropScheduledMessagesWindowMinSeconds: "300",
		ABPropScheduledMessagesWindowMaxSeconds: "86400",
	})
	if !cfg.Known || !cfg.SenderEnabled || cfg.MinWindow != 5*time.Minute || cfg.MaxWindow != 24*time.Hour {
		t.Fatalf("from props %+v", cfg)
	}
}

func TestScheduledMessageMexNotifications(t *testing.T) {
	cli := scheduledTestClient()
	var got []any
	cli.AddEventHandler(func(evt any) { got = append(got, evt) })
	key := bytes.Repeat([]byte{0xFE}, 32)
	from := types.NewJID("100000000000002", types.HiddenUserServer)
	mex := func(op, payload string) *waBinary.Node {
		return &waBinary.Node{
			Tag:   "notification",
			Attrs: waBinary.Attrs{"type": "mex", "from": from, "id": "1", "t": "1794281400"},
			Content: []waBinary.Node{{
				Tag:     "update",
				Attrs:   waBinary.Attrs{"op_name": op},
				Content: []byte(payload),
			}},
		}
	}
	rk := base64.RawURLEncoding.EncodeToString(key)
	cli.handleMexNotification(context.Background(), mex("NotificationScheduledMessageReveal",
		fmt.Sprintf(`{"data":{"xwa2_notify_scheduled_message_reveal":{"chat_jid":"%s","rk":"%s","rkid":"R1","st":1794281400}}}`, from, rk)))
	cli.handleMexNotification(context.Background(), mex("NotificationScheduledMessageReveal",
		fmt.Sprintf(`{"data":{"xwa2_notify_scheduled_message_reveal":{"rk":"%s","rkid":"R2","st":"1794281400"}}}`, base64.URLEncoding.EncodeToString(key))))
	cli.handleMexNotification(context.Background(), mex("NotificationScheduledMessagePost",
		`{"data":{"xwa2_notify_scheduled_message_post":{"rkid":"R1","status":"SUCCESS"}}}`))
	if len(got) != 3 {
		t.Fatalf("got %d events: %#v", len(got), got)
	}
	for i, rkid := range []string{"R1", "R2"} {
		reveal, ok := got[i].(*events.ScheduledMessageReveal)
		if !ok || reveal.RevealKeyID != rkid || !bytes.Equal(reveal.RevealKey, key) || reveal.Time.Unix() != 1794281400 || reveal.From != from {
			t.Fatalf("reveal %d: %#v", i, got[i])
		}
	}
	if got[0].(*events.ScheduledMessageReveal).Chat != from || !got[1].(*events.ScheduledMessageReveal).Chat.IsEmpty() {
		t.Fatal("chat_jid not read")
	}
	post, ok := got[2].(*events.ScheduledMessagePost)
	if !ok || post.RevealKeyID != "R1" || !post.IsSuccess() || post.Mex.OpName != "NotificationScheduledMessagePost" {
		t.Fatalf("post %#v", got[2])
	}
}
