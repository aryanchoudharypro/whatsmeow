package whatsmeow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/util/random"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// EventCreation is the content of a new event (a group or chat calendar
// event). Only Name is required.
type EventCreation struct {
	Name               string
	Description        string
	StartTime, EndTime time.Time
	JoinLink           string
	Location           *waE2E.LocationMessage
	IsScheduledCall    bool
	ExtraGuestsAllowed bool
}

// BuildEventCreation builds an event message. Like a poll it carries a
// message secret, which the RSVPs to it are encrypted with; the official
// apps reject an event without one. The built message can be sent normally
// using Client.SendMessage.
//
// Ported from whatsapp-rust's Events::create.
func (cli *Client) BuildEventCreation(event EventCreation) (*waE2E.Message, error) {
	if strings.TrimSpace(event.Name) == "" {
		return nil, fmt.Errorf("event name must not be empty")
	}
	msg := &waE2E.EventMessage{
		Name:     proto.String(event.Name),
		Location: event.Location,
	}
	if event.Description != "" {
		msg.Description = proto.String(event.Description)
	}
	if !event.StartTime.IsZero() {
		msg.StartTime = proto.Int64(event.StartTime.Unix())
	}
	if !event.EndTime.IsZero() {
		msg.EndTime = proto.Int64(event.EndTime.Unix())
	}
	if event.JoinLink != "" {
		msg.JoinLink = proto.String(event.JoinLink)
	}
	if event.IsScheduledCall {
		msg.IsScheduleCall = proto.Bool(true)
	}
	if event.ExtraGuestsAllowed {
		msg.ExtraGuestsAllowed = proto.Bool(true)
	}
	return &waE2E.Message{
		EventMessage:       msg,
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: random.Bytes(32)},
	}, nil
}

// EncryptEventResponse encrypts an RSVP to the event described by eventInfo.
// This is a slightly lower-level function, using BuildEventResponse is
// recommended.
func (cli *Client) EncryptEventResponse(ctx context.Context, eventInfo *types.MessageInfo, response *waE2E.EventResponseMessage) (*waE2E.EncEventResponseMessage, error) {
	plaintext, err := proto.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal event response protobuf: %w", err)
	}
	// The responder's JID keys the encryption, in the event creator's
	// namespace: our LID for a LID-addressed event, our phone number otherwise.
	ownID := cli.getOwnLID()
	if eventInfo.Sender.Server == types.DefaultUserServer || ownID.IsEmpty() {
		ownID = cli.getOwnID()
	}
	ciphertext, iv, err := cli.encryptMsgSecret(ctx, ownID, eventInfo.Chat, eventInfo.Sender, eventInfo.ID, EncSecretEventResponse, plaintext)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt event response: %w", err)
	}
	return &waE2E.EncEventResponseMessage{
		EventCreationMessageKey: getKeyFromInfo(eventInfo),
		EncPayload:              ciphertext,
		EncIV:                   iv,
	}, nil
}

// BuildEventResponse builds an RSVP (going, not going, maybe) to an event,
// optionally bringing extraGuests more people. The built message can be sent
// normally using Client.SendMessage to the event's chat.
//
// Ported from whatsapp-rust's Events::respond.
func (cli *Client) BuildEventResponse(ctx context.Context, eventInfo *types.MessageInfo, response waE2E.EventResponseMessage_EventResponseType, extraGuests int) (*waE2E.Message, error) {
	resp := &waE2E.EventResponseMessage{
		Response:    response.Enum(),
		TimestampMS: proto.Int64(time.Now().UnixMilli()),
	}
	if extraGuests > 0 {
		resp.ExtraGuestCount = proto.Int32(int32(extraGuests))
	}
	enc, err := cli.EncryptEventResponse(ctx, eventInfo, resp)
	if err != nil {
		return nil, err
	}
	return &waE2E.Message{EncEventResponseMessage: enc}, nil
}

// DecryptEventResponse decrypts an RSVP to an event. The event's creation
// message must have been seen before (its message secret is needed).
func (cli *Client) DecryptEventResponse(ctx context.Context, evt *events.Message) (*waE2E.EventResponseMessage, error) {
	enc := evt.Message.GetEncEventResponseMessage()
	if enc == nil {
		return nil, fmt.Errorf("message is not an event response")
	}
	plaintext, err := cli.decryptMsgSecret(ctx, evt, EncSecretEventResponse, enc, enc.GetEventCreationMessageKey())
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt event response: %w", err)
	}
	var resp waE2E.EventResponseMessage
	if err = proto.Unmarshal(plaintext, &resp); err != nil {
		return nil, fmt.Errorf("failed to decode event response protobuf: %w", err)
	}
	return &resp, nil
}
