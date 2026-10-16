// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// Field numbers used to walk a history sync blob without decoding it, read
// from the generated descriptors so a proto update can't silently shift them.
var (
	hsFieldConversations = fieldNumber((&waHistorySync.HistorySync{}).ProtoReflect(), "conversations")

	convFieldID          = fieldNumber((&waHistorySync.Conversation{}).ProtoReflect(), "ID")
	convFieldMessages    = fieldNumber((&waHistorySync.Conversation{}).ProtoReflect(), "messages")
	convFieldTCToken     = fieldNumber((&waHistorySync.Conversation{}).ProtoReflect(), "tcToken")
	convFieldTCTokenTS   = fieldNumber((&waHistorySync.Conversation{}).ProtoReflect(), "tcTokenTimestamp")
	convFieldTCTokenSend = fieldNumber((&waHistorySync.Conversation{}).ProtoReflect(), "tcTokenSenderTimestamp")

	hsMsgFieldMessage = fieldNumber((&waHistorySync.HistorySyncMsg{}).ProtoReflect(), "message")

	wmiFieldKey         = fieldNumber((&waWeb.WebMessageInfo{}).ProtoReflect(), "key")
	wmiFieldSecret      = fieldNumber((&waWeb.WebMessageInfo{}).ProtoReflect(), "messageSecret")
	wmiFieldParticipant = fieldNumber((&waWeb.WebMessageInfo{}).ProtoReflect(), "participant")

	keyFieldFromMe      = fieldNumber((&waCommon.MessageKey{}).ProtoReflect(), "fromMe")
	keyFieldID          = fieldNumber((&waCommon.MessageKey{}).ProtoReflect(), "ID")
	keyFieldParticipant = fieldNumber((&waCommon.MessageKey{}).ProtoReflect(), "participant")
)

func fieldNumber(msg protoreflect.Message, name protoreflect.Name) protowire.Number {
	fd := msg.Descriptor().Fields().ByName(name)
	if fd == nil {
		panic(fmt.Sprintf("whatsmeow: %s has no field %s", msg.Descriptor().FullName(), name))
	}
	return fd.Number()
}

var errMalformedHistorySync = errors.New("malformed history sync protobuf")

// walkFields calls fn for each top-level field of an encoded message. For
// length-delimited fields val is the payload; for varints num holds the value.
func walkFields(b []byte, fn func(field protowire.Number, typ protowire.Type, val []byte, num uint64) error) error {
	for len(b) > 0 {
		field, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return errMalformedHistorySync
		}
		b = b[n:]
		var val []byte
		var num uint64
		switch typ {
		case protowire.BytesType:
			val, n = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			num, n = protowire.ConsumeVarint(b)
		default:
			n = protowire.ConsumeFieldValue(field, typ, b)
		}
		if n < 0 {
			return errMalformedHistorySync
		}
		b = b[n:]
		if err := fn(field, typ, val, num); err != nil {
			return err
		}
	}
	return nil
}

// splitHistorySync decodes everything in a history sync blob except its
// conversations, which are returned still encoded (as views into raw).
func splitHistorySync(raw []byte) (*waHistorySync.HistorySync, [][]byte, error) {
	rest := make([]byte, 0, 1024)
	var convs [][]byte
	err := walkFields(raw, func(field protowire.Number, typ protowire.Type, val []byte, _ uint64) error {
		if field == hsFieldConversations && typ == protowire.BytesType {
			convs = append(convs, val)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	// Re-walk to copy the other fields verbatim, preserving their order.
	for b := raw; len(b) > 0; {
		field, typ, n := protowire.ConsumeTag(b)
		m := protowire.ConsumeFieldValue(field, typ, b[n:])
		if field != hsFieldConversations {
			rest = append(rest, b[:n+m]...)
		}
		b = b[n+m:]
	}
	var hs waHistorySync.HistorySync
	if err = proto.Unmarshal(rest, &hs); err != nil {
		return nil, nil, err
	}
	return &hs, convs, nil
}

// historyMessageSecretRef is what storing a message secret needs from one
// history message.
type historyMessageSecretRef struct {
	id          string
	fromMe      bool
	keyPart     string
	participant string
	secret      []byte
}

// historyConversationScan is what storing message secrets and privacy tokens
// needs from one conversation, read without decoding the message bodies.
type historyConversationScan struct {
	id         string
	tcToken    []byte
	tcTokenTS  uint64
	tcSenderTS uint64
	messages   int
	secrets    []historyMessageSecretRef
}

func scanHistoryConversation(raw []byte) (scan historyConversationScan, err error) {
	err = walkFields(raw, func(field protowire.Number, _ protowire.Type, val []byte, num uint64) error {
		switch field {
		case convFieldID:
			scan.id = string(val)
		case convFieldTCToken:
			scan.tcToken = val
		case convFieldTCTokenTS:
			scan.tcTokenTS = num
		case convFieldTCTokenSend:
			scan.tcSenderTS = num
		case convFieldMessages:
			scan.messages++
			return walkFields(val, func(field protowire.Number, _ protowire.Type, wmi []byte, _ uint64) error {
				if field != hsMsgFieldMessage {
					return nil
				}
				var ref historyMessageSecretRef
				err := walkFields(wmi, func(field protowire.Number, _ protowire.Type, val []byte, _ uint64) error {
					switch field {
					case wmiFieldSecret:
						ref.secret = val
					case wmiFieldParticipant:
						ref.participant = string(val)
					case wmiFieldKey:
						return walkFields(val, func(field protowire.Number, _ protowire.Type, val []byte, num uint64) error {
							switch field {
							case keyFieldFromMe:
								ref.fromMe = num != 0
							case keyFieldID:
								ref.id = string(val)
							case keyFieldParticipant:
								ref.keyPart = string(val)
							}
							return nil
						})
					}
					return nil
				})
				if err == nil && ref.secret != nil {
					scan.secrets = append(scan.secrets, ref)
				}
				return err
			})
		}
		return nil
	})
	return
}

// storeHistoricalMessageSecretsEncoded is storeHistoricalMessageSecrets for
// conversations that are still encoded. It reads only the IDs, keys,
// secrets and privacy tokens, skipping every message body, so storing them
// doesn't cost a second full decode of the chunk.
func (cli *Client) storeHistoricalMessageSecretsEncoded(ctx context.Context, convs [][]byte) {
	ownID := cli.getOwnID().ToNonAD()
	if ownID.IsEmpty() {
		return
	}
	var secrets []store.MessageSecretInsert
	var privacyTokens []store.PrivacyToken
	for i, raw := range convs {
		scan, err := scanHistoryConversation(raw)
		if err != nil {
			cli.Log.Warnf("Failed to scan history sync conversation %d for message secrets: %v", i, err)
			continue
		}
		chatJID, _ := types.ParseJID(scan.id)
		if chatJID.IsEmpty() {
			continue
		}
		var userJID types.JID
		if chatJID.Server == types.HiddenUserServer {
			userJID = chatJID
		} else if chatJID.Server == types.DefaultUserServer {
			userJID, _ = cli.Store.LIDs.GetLIDForPN(ctx, chatJID)
			if userJID.IsEmpty() {
				userJID = chatJID
			}
		}
		if !userJID.IsEmpty() && scan.tcToken != nil {
			privacyTokens = append(privacyTokens, store.PrivacyToken{
				User:            userJID,
				Token:           scan.tcToken,
				Timestamp:       time.Unix(int64(scan.tcTokenTS), 0),
				SenderTimestamp: time.Unix(int64(scan.tcSenderTS), 0),
			})
		}
		for _, ref := range scan.secrets {
			var senderJID types.JID
			if ref.fromMe {
				senderJID = ownID
			} else if chatJID.Server == types.DefaultUserServer || chatJID.Server == types.HiddenUserServer {
				senderJID = chatJID
			} else if ref.keyPart != "" {
				senderJID, _ = types.ParseJID(ref.keyPart)
			} else if ref.participant != "" {
				senderJID, _ = types.ParseJID(ref.participant)
			}
			if senderJID.IsEmpty() || ref.id == "" {
				continue
			}
			secrets = append(secrets, store.MessageSecretInsert{
				Chat:   chatJID,
				Sender: senderJID,
				ID:     ref.id,
				Secret: ref.secret,
			})
		}
	}
	cli.putHistoricalSecretsAndTokens(ctx, secrets, privacyTokens)
}

// countHistoryMessages counts the messages in encoded conversations.
func countHistoryMessages(convs [][]byte) (total int) {
	for _, raw := range convs {
		_ = walkFields(raw, func(field protowire.Number, _ protowire.Type, _ []byte, _ uint64) error {
			if field == convFieldMessages {
				total++
			}
			return nil
		})
	}
	return
}
