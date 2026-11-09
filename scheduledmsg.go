// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/util/random"
	"google.golang.org/protobuf/proto"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// Scheduled message constants, as WA Web's WAWebScheduledMsgConstants has them.
const (
	// ScheduledMessageMetaType is the <meta type> of a scheduled message stanza.
	ScheduledMessageMetaType = "scheduled_message"
	// ScheduledMessageMaxPerChat is how many pending scheduled messages a chat may have.
	ScheduledMessageMaxPerChat = 30
	// ScheduledMessageMaxMedia is how many media messages one scheduling may carry.
	ScheduledMessageMaxMedia = 1
	// ScheduledMessageLimitNackCode is the error the server acks a scheduled
	// message with when the chat (or account) is at its limit.
	ScheduledMessageLimitNackCode = 419
	// ScheduledMessageRevealKeyRetention is how long WA Web keeps reveal keys
	// (and orphan reveal keys) before dropping them.
	ScheduledMessageRevealKeyRetention = 30 * 24 * time.Hour

	scheduledMessageRevealKeyBytes = 32
	scheduledMessageIVBytes        = 12
)

// Server feature flags for scheduled messages, by config code, as WA Web's
// WAWebScheduledMessagesGatingUtils and WAWebABPropsConfigs name them.
const (
	ABPropScheduledMessagesSenderEnabled          = 23845 // scheduled_messages_sender_enabled (default false)
	ABPropScheduledMessagesReceiverEnabled        = 24610 // scheduled_messages_receiver_enabled (default false)
	ABPropScheduledMessagesWindowMaxSeconds       = 26347 // scheduled_messages_window_duration_max_seconds (default 1209600)
	ABPropScheduledMessagesWindowMinSeconds       = 26348 // scheduled_messages_window_duration_min_seconds (default 600)
	ABPropScheduledMessagesPhotoVideoSenderEnable = 32553 // scheduled_messages_photo_video_sender_enabled (default false)
	ABPropScheduledMessagesSmartDefaults          = 36583 // scheduled_messages_smart_defaults (default false)
)

// The window WA Web falls back to when the server didn't send one.
const (
	DefaultScheduledMessageMinWindow = 600 * time.Second
	DefaultScheduledMessageMaxWindow = 1209600 * time.Second
)

var (
	// ErrScheduledMessageLimitReached is returned when the server refuses a
	// scheduled message with error 419 (too many scheduled messages).
	ErrScheduledMessageLimitReached = errors.New("scheduled message limit reached")
	// ErrScheduledMessageUnsupported is returned for content or chats WA Web
	// doesn't let you schedule.
	ErrScheduledMessageUnsupported = errors.New("message can't be scheduled")
	// ErrInvalidRevealKey is returned when a reveal key isn't 32 bytes.
	ErrInvalidRevealKey = errors.New("invalid scheduled message reveal key")
)

// Int reads a numeric flag, with def when the server didn't send it or it isn't a number.
func (props ABProps) Int(code int, def int64) int64 {
	value, ok := props[code]
	if !ok {
		return def
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return def
	}
	return parsed
}

// ScheduledMessagesConfig is what the server's flags say about scheduled
// messages. Known is false when the flags haven't been fetched yet.
type ScheduledMessagesConfig struct {
	Known                   bool
	SenderEnabled           bool
	ReceiverEnabled         bool
	PhotoVideoSenderEnabled bool
	SmartDefaults           bool
	MinWindow               time.Duration
	MaxWindow               time.Duration
}

// ScheduledMessagesConfig reads the scheduled message flags from the cached
// AB props, with WA Web's defaults for anything missing.
func (cli *Client) ScheduledMessagesConfig() ScheduledMessagesConfig {
	return scheduledMessagesConfigFromProps(cli.CachedABProps())
}

func scheduledMessagesConfigFromProps(props ABProps) ScheduledMessagesConfig {
	cfg := ScheduledMessagesConfig{
		Known:                   len(props) > 0,
		SenderEnabled:           props.Bool(ABPropScheduledMessagesSenderEnabled, false),
		ReceiverEnabled:         props.Bool(ABPropScheduledMessagesReceiverEnabled, false),
		PhotoVideoSenderEnabled: props.Bool(ABPropScheduledMessagesPhotoVideoSenderEnable, false),
		SmartDefaults:           props.Bool(ABPropScheduledMessagesSmartDefaults, false),
		MinWindow:               time.Duration(props.Int(ABPropScheduledMessagesWindowMinSeconds, int64(DefaultScheduledMessageMinWindow/time.Second))) * time.Second,
		MaxWindow:               time.Duration(props.Int(ABPropScheduledMessagesWindowMaxSeconds, int64(DefaultScheduledMessageMaxWindow/time.Second))) * time.Second,
	}
	return cfg
}

// IsScheduledTimeValid reports whether at is inside the scheduling window,
// as WA Web's WAWebIsScheduledTimeValid checks it: at least minWindow after
// the start of the current minute and at most maxWindow after now.
func IsScheduledTimeValid(at, now time.Time, minWindow, maxWindow time.Duration) bool {
	scheduled := at.Unix()
	nowS := now.Unix()
	minuteStart := (nowS / 60) * 60
	return scheduled-minuteStart >= int64(minWindow/time.Second) && scheduled-nowS <= int64(maxWindow/time.Second)
}

// DefaultScheduledMessageTime is the time WA Web's schedule picker
// (WAWebScheduleTimePicker) starts on: the first whole minute that is at
// least minWindow away, or with smart defaults on
// (WAWebScheduledMsgSmartDefaultTime) 9:00 or 17:00 local time, clamped to
// the window.
func DefaultScheduledMessageTime(now time.Time, minWindow, maxWindow time.Duration, smartDefaults bool) time.Time {
	nowS := now.Unix()
	minS := max(int64(minWindow/time.Second), 0)
	minuteStart := (nowS / 60) * 60
	earliest := ceilDiv(minuteStart+minS, 60) * 60
	nextMinute := ceilDiv(nowS+1, 60) * 60
	lower := max(earliest, nextMinute)
	if !smartDefaults {
		return time.Unix(lower, 0).In(now.Location())
	}
	upper := nowS + int64(maxWindow/time.Second)
	at := func(days, hour int) int64 {
		y, m, d := now.Date()
		return time.Date(y, m, d+days, hour, 0, 0, 0, now.Location()).Unix()
	}
	clamp := func(t int64) time.Time {
		return time.Unix(max(min(t, upper), lower), 0).In(now.Location())
	}
	minuteOfDay := now.Hour()*60 + now.Minute()
	switch {
	case minuteOfDay < 8*60+30:
		return clamp(at(0, 9))
	case minuteOfDay < 16*60+30:
		return clamp(at(0, 17))
	default:
		return clamp(at(1, 9))
	}
}

func ceilDiv(a, b int64) int64 {
	if a >= 0 {
		return (a + b - 1) / b
	}
	return a / b
}

// GenerateRevealKey makes a new reveal key and its ID the way WA Web's
// WAWebScheduledMsgCrypto does: 32 random bytes and a random UUID.
func GenerateRevealKey() (key []byte, keyID string) {
	return random.Bytes(scheduledMessageRevealKeyBytes), uuid.NewString()
}

// EncryptWithRevealKey seals plaintext with AES-256-GCM under a random
// 12-byte IV (WAWebScheduledMsgCrypto.encryptWithRevealKey).
func EncryptWithRevealKey(plaintext, revealKey []byte) (iv, ciphertext []byte, err error) {
	gcm, err := revealKeyCipher(revealKey)
	if err != nil {
		return nil, nil, err
	}
	iv = random.Bytes(scheduledMessageIVBytes)
	return iv, gcm.Seal(nil, iv, plaintext, nil), nil
}

// DecryptWithRevealKey opens what EncryptWithRevealKey sealed
// (WAWebScheduledMsgCrypto.decryptWithRevealKey).
func DecryptWithRevealKey(ciphertext, iv, revealKey []byte) ([]byte, error) {
	gcm, err := revealKeyCipher(revealKey)
	if err != nil {
		return nil, err
	}
	if len(iv) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid scheduled message IV length %d", len(iv))
	}
	return gcm.Open(nil, iv, ciphertext, nil)
}

func revealKeyCipher(revealKey []byte) (cipher.AEAD, error) {
	if len(revealKey) != scheduledMessageRevealKeyBytes {
		return nil, ErrInvalidRevealKey
	}
	block, err := aes.NewCipher(revealKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// PreparedScheduledMessage is a message wrapped for scheduling.
type PreparedScheduledMessage struct {
	// Wrapped is the ConditionalRevealMessage to send in place of the message.
	Wrapped *waE2E.Message
	// Inner is the message as encrypted inside Wrapped (with its message secret).
	Inner          *waE2E.Message
	InnerPlaintext []byte
	RevealKey      []byte
	RevealKeyID    string
	Time           time.Time
}

// PrepareScheduledMessage wraps a message for sending at a later time, the
// way WA Web's WAWebScheduledMsgSenderJob.prepareScheduledMessage does: the
// message (given a message secret if it has none) is encrypted with a new
// reveal key into a ConditionalRevealMessage, and the wrapper carries the
// same secret.
func PrepareScheduledMessage(inner *waE2E.Message, at time.Time) (*PreparedScheduledMessage, error) {
	if inner == nil {
		return nil, fmt.Errorf("%w: no message", ErrScheduledMessageUnsupported)
	}
	inner = proto.Clone(inner).(*waE2E.Message)
	if inner.MessageContextInfo == nil {
		inner.MessageContextInfo = &waE2E.MessageContextInfo{}
	}
	if len(inner.MessageContextInfo.MessageSecret) == 0 {
		inner.MessageContextInfo.MessageSecret = random.Bytes(32)
	}
	plaintext, err := proto.Marshal(inner)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal scheduled message: %w", err)
	}
	key, keyID := GenerateRevealKey()
	iv, payload, err := EncryptWithRevealKey(plaintext, key)
	if err != nil {
		return nil, err
	}
	return &PreparedScheduledMessage{
		Wrapped: &waE2E.Message{
			ConditionalRevealMessage: &waE2E.ConditionalRevealMessage{
				ConditionalRevealMessageType: waE2E.ConditionalRevealMessage_SCHEDULED_MESSAGE.Enum(),
				EncIV:                        iv,
				EncPayload:                   payload,
				RevealKeyID:                  proto.String(keyID),
			},
			MessageContextInfo: &waE2E.MessageContextInfo{
				MessageSecret: inner.MessageContextInfo.MessageSecret,
			},
		},
		Inner:          inner,
		InnerPlaintext: plaintext,
		RevealKey:      key,
		RevealKeyID:    keyID,
		Time:           time.Unix(at.Unix(), 0),
	}, nil
}

// DecryptScheduledMessage opens a ConditionalRevealMessage with its reveal
// key. Like WA Web's WAWebScheduledMsgDecryptInnerProto, a payload that is
// an associatedChildMessage is unwrapped, keeping the outer context info.
func DecryptScheduledMessage(crm *waE2E.ConditionalRevealMessage, revealKey []byte) (*waE2E.Message, error) {
	if crm == nil || crm.EncPayload == nil || crm.EncIV == nil || crm.RevealKeyID == nil {
		return nil, errors.New("conditional reveal message is missing fields")
	}
	plaintext, err := DecryptWithRevealKey(crm.GetEncPayload(), crm.GetEncIV(), revealKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt scheduled message: %w", err)
	}
	var msg waE2E.Message
	if err = proto.Unmarshal(plaintext, &msg); err != nil {
		return nil, fmt.Errorf("failed to decode scheduled message: %w", err)
	}
	if child := msg.GetAssociatedChildMessage().GetMessage(); child != nil {
		child = proto.Clone(child).(*waE2E.Message)
		child.MessageContextInfo = msg.MessageContextInfo
		return child, nil
	}
	return &msg, nil
}

// ExtractScheduledMessageText is the text of a scheduled message, as WA
// Web's WAWebScheduledMsgExtractText finds it (conversation or extended
// text only).
func ExtractScheduledMessageText(msg *waE2E.Message) (string, bool) {
	if msg.Conversation != nil {
		return msg.GetConversation(), true
	}
	if msg.GetExtendedTextMessage().Text != nil {
		return msg.GetExtendedTextMessage().GetText(), true
	}
	return "", false
}

// parseScheduledMessageMeta reads <meta type="scheduled_message" st><key rkid>
// the way WA Web's WAWebHandleMsgParser.parseMessageMeta does, except that a
// missing st or key content is allowed (an unschedule only names the key).
func parseScheduledMessageMeta(metaNode waBinary.Node) (*types.ScheduledMessageMeta, error) {
	ag := metaNode.AttrGetter()
	meta := &types.ScheduledMessageMeta{}
	if st, ok := ag.GetInt64("st", false); ok && st > 0 {
		meta.Time = time.Unix(st, 0)
	}
	keyNode, ok := metaNode.GetOptionalChildByTag("key")
	if !ok {
		return meta, errors.New("scheduled_message meta has no key")
	}
	meta.RevealKeyID = keyNode.AttrGetter().OptionalString("rkid")
	if meta.RevealKeyID == "" {
		return meta, errors.New("scheduled_message key has no rkid")
	}
	if content, ok := keyNode.Content.([]byte); ok && len(content) > 0 {
		meta.RevealKey = decodeRevealKey(content)
		if meta.RevealKey == nil {
			return meta, fmt.Errorf("scheduled_message reveal key has unexpected length %d", len(content))
		}
	}
	return meta, nil
}

// decodeRevealKey takes the 32 raw bytes, or 64 hex characters, WA Web accepts.
func decodeRevealKey(content []byte) []byte {
	switch len(content) {
	case scheduledMessageRevealKeyBytes:
		return content
	case scheduledMessageRevealKeyBytes * 2:
		decoded, err := hex.DecodeString(string(content))
		if err == nil && len(decoded) == scheduledMessageRevealKeyBytes {
			return decoded
		}
	}
	return nil
}

// scheduledSendMeta is what SendMessage adds to a scheduled message stanza
// (WA Web's scheduledMsgMetadata).
type scheduledSendMeta struct {
	unschedule     bool
	time           time.Time
	revealKeyID    string
	revealKey      []byte
	stanzaType     string
	mediaType      string
	innerPlaintext []byte
}

// addTo fills in <meta type="scheduled_message" st><key rkid>key</key></meta>
// (WAWebScheduledMsgStanzaContributor and WAWebSendMsgMetaNode); an
// unschedule names only the key ID.
func (sm *scheduledSendMeta) addTo(meta *waBinary.Node) {
	meta.Attrs["type"] = ScheduledMessageMetaType
	keyNode := waBinary.Node{Tag: "key", Attrs: waBinary.Attrs{"rkid": sm.revealKeyID}}
	if !sm.unschedule {
		meta.Attrs["st"] = strconv.FormatInt(sm.time.Unix(), 10)
		keyNode.Content = sm.revealKey
	}
	meta.Content = []waBinary.Node{keyNode}
}

// ScheduledSendResponse is the result of SendScheduledMessage.
type ScheduledSendResponse struct {
	SendResponse
	// What the caller needs to keep to list, unschedule or send the message
	// now later: the reveal key, its ID and the wrapped message as sent.
	RevealKey   []byte
	RevealKeyID string
	Time        time.Time
	Wrapped     *waE2E.Message
	Inner       *waE2E.Message
}

// checkSchedulable applies WA Web's limits: text, or one non-view-once image
// or video (WAWebSendScheduledMsgAction, SCHEDULABLE_MEDIA_TYPES), to a
// person or group chat (WAWebIsScheduledMessagesAvailableForChat).
func checkSchedulable(to types.JID, msg *waE2E.Message) error {
	switch to.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
	default:
		return fmt.Errorf("%w: can't schedule messages to %s", ErrScheduledMessageUnsupported, to.Server)
	}
	if to.IsBot() {
		return fmt.Errorf("%w: can't schedule messages to bots", ErrScheduledMessageUnsupported)
	}
	switch {
	case msg.Conversation != nil, msg.ExtendedTextMessage != nil:
		return nil
	case msg.ImageMessage != nil:
		if msg.ImageMessage.GetViewOnce() {
			return fmt.Errorf("%w: view-once media", ErrScheduledMessageUnsupported)
		}
		return nil
	case msg.VideoMessage != nil:
		if msg.VideoMessage.GetViewOnce() {
			return fmt.Errorf("%w: view-once media", ErrScheduledMessageUnsupported)
		}
		return nil
	default:
		return fmt.Errorf("%w: only text, photos and videos can be scheduled", ErrScheduledMessageUnsupported)
	}
}

// SendScheduledMessage sends a message that the server delivers at the given
// time, the way WA Web's WAWebSendScheduledMsgAction and
// WAWebScheduledMsgSenderJob do: the message is encrypted with a new reveal
// key into a ConditionalRevealMessage, and the stanza says when to reveal it
// and carries the key in <meta type="scheduled_message">. The message type
// and media type attributes are the original message's.
//
// Keep the returned reveal key and wrapped message: they are what lists,
// unschedules or sends the message now later. WA Web allows 30 pending
// scheduled messages per chat (ScheduledMessageMaxPerChat), which the caller
// has to count; the server refuses more with ErrScheduledMessageLimitReached.
func (cli *Client) SendScheduledMessage(ctx context.Context, to types.JID, message *waE2E.Message, at time.Time, extra ...SendRequestExtra) (resp ScheduledSendResponse, err error) {
	if cli == nil {
		err = ErrClientIsNil
		return
	}
	if err = checkSchedulable(to, message); err != nil {
		return
	}
	var req SendRequestExtra
	if len(extra) > 1 {
		err = errors.New("only one extra parameter may be provided to SendScheduledMessage")
		return
	} else if len(extra) == 1 {
		req = extra[0]
	}
	inner := proto.Clone(message).(*waE2E.Message)
	if cli.shouldIncludeReportingToken(inner) {
		if inner.MessageContextInfo == nil {
			inner.MessageContextInfo = &waE2E.MessageContextInfo{}
		}
		inner.MessageContextInfo.ReportingTokenVersion = proto.Int32(reportingTokenVersion)
	}
	prepared, err := PrepareScheduledMessage(inner, at)
	if err != nil {
		return
	}
	req.scheduled = &scheduledSendMeta{
		time:           prepared.Time,
		revealKeyID:    prepared.RevealKeyID,
		revealKey:      prepared.RevealKey,
		stanzaType:     getTypeFromMessage(prepared.Inner),
		mediaType:      getMediaTypeFromMessage(prepared.Inner),
		innerPlaintext: prepared.InnerPlaintext,
	}
	resp.RevealKey = prepared.RevealKey
	resp.RevealKeyID = prepared.RevealKeyID
	resp.Time = prepared.Time
	resp.Wrapped = prepared.Wrapped
	resp.Inner = prepared.Inner
	resp.SendResponse, err = cli.SendMessage(ctx, to, prepared.Wrapped, req)
	return
}

// BuildUnschedule builds the protocol message that cancels one of your
// scheduled messages (WAWebGenerateProtocolUnscheduleMessageProto). Send it
// with UnscheduleMessage, which adds the reveal key ID the server needs.
func (cli *Client) BuildUnschedule(chat types.JID, id types.MessageID) *waE2E.Message {
	return &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_UNSCHEDULE.Enum(),
			Key:  cli.BuildMessageKey(chat, types.EmptyJID, id),
		},
	}
}

// UnscheduleMessage cancels one of your scheduled messages, the way WA Web's
// WAWebUnscheduleMsgAction and WAWebScheduledMsgUnscheduleDirectSend do: a
// MESSAGE_UNSCHEDULE protocol message with edit="7" and
// <meta type="scheduled_message"><key rkid/></meta>.
func (cli *Client) UnscheduleMessage(ctx context.Context, chat types.JID, id types.MessageID, revealKeyID string) (SendResponse, error) {
	if revealKeyID == "" {
		return SendResponse{}, errors.New("unscheduling needs the reveal key ID")
	}
	return cli.SendMessage(ctx, chat, cli.BuildUnschedule(chat, id), SendRequestExtra{
		scheduled: &scheduledSendMeta{unschedule: true, revealKeyID: revealKeyID},
	})
}

// SendScheduledMessageNow sends the text of a scheduled message as a normal
// message, the way WA Web's WAWebSendScheduledMsgNowAction does (it offers
// it for scheduled messages that failed). Only text can be recovered this
// way; the scheduled copy itself is not touched, so unschedule it first if
// it is still pending.
func (cli *Client) SendScheduledMessageNow(ctx context.Context, chat types.JID, crm *waE2E.ConditionalRevealMessage, revealKey []byte) (SendResponse, error) {
	inner, err := DecryptScheduledMessage(crm, revealKey)
	if err != nil {
		return SendResponse{}, err
	}
	text, ok := ExtractScheduledMessageText(inner)
	if !ok || text == "" {
		return SendResponse{}, errors.New("cannot recover the scheduled message's text")
	}
	return cli.SendMessage(ctx, chat, &waE2E.Message{Conversation: proto.String(text)})
}
