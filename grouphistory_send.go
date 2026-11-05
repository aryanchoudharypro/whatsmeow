package whatsmeow

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waGroupHistory"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
)

// SendGroupHistory sends recent group messages to people who joined the
// group, like WA Web's "Send message history": a zlib-compressed
// GroupHistory bundle uploaded as group-history media, a
// messageHistoryBundle message to the group naming the receivers, then a
// hidden messageHistoryNotice. messages should be newest first, at most 100,
// as WA Web picks them (WAWebRetrieveMessagesForBundle).
func (cli *Client) SendGroupHistory(ctx context.Context, group types.JID, receivers, nonReceivers []types.JID, messages []*waWeb.WebMessageInfo) error {
	if len(receivers) == 0 || len(messages) == 0 {
		return fmt.Errorf("nothing to send")
	}
	encoded, err := proto.Marshal(&waGroupHistory.GroupHistory{Messages: messages})
	if err != nil {
		return fmt.Errorf("failed to encode history: %w", err)
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err = zw.Write(encoded); err == nil {
		err = zw.Close()
	}
	if err != nil {
		return fmt.Errorf("failed to compress history: %w", err)
	}
	upload, err := cli.Upload(ctx, compressed.Bytes(), MediaGroupHistory)
	if err != nil {
		return fmt.Errorf("failed to upload history: %w", err)
	}
	oldest := int64(messages[len(messages)-1].GetMessageTimestamp())
	jids := func(list []types.JID) []string {
		out := make([]string, len(list))
		for i, jid := range list {
			out[i] = jid.ToNonAD().String()
		}
		return out
	}
	metadata := func() *waE2E.MessageHistoryMetadata {
		return &waE2E.MessageHistoryMetadata{
			HistoryReceivers:               jids(receivers),
			NonHistoryReceivers:            jids(nonReceivers),
			MessageCount:                   proto.Int64(int64(len(messages))),
			OldestMessageTimestampInWindow: proto.Int64(oldest),
			OldestMessageTimestampInBundle: proto.Int64(oldest),
		}
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	bundle := &waE2E.Message{
		MessageHistoryBundle: &waE2E.MessageHistoryBundle{
			Mimetype:               proto.String("application/protobuf"),
			FileSHA256:             upload.FileSHA256,
			FileEncSHA256:          upload.FileEncSHA256,
			MediaKey:               upload.MediaKey,
			DirectPath:             proto.String(upload.DirectPath),
			MediaKeyTimestamp:      proto.Int64(time.Now().Unix()),
			MessageHistoryMetadata: metadata(),
		},
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: secret},
	}
	if _, err = cli.SendMessage(ctx, group, bundle); err != nil {
		return fmt.Errorf("failed to send history bundle: %w", err)
	}
	notice := &waE2E.Message{
		MessageHistoryNotice: &waE2E.MessageHistoryNotice{MessageHistoryMetadata: metadata()},
	}
	if _, err = cli.SendMessage(ctx, group, notice); err != nil {
		return fmt.Errorf("failed to send history notice: %w", err)
	}
	return nil
}
