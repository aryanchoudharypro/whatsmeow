package whatsmeow

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
)

func TestDescribeHistorySyncBlob(t *testing.T) {
	hs := &waHistorySync.HistorySync{
		SyncType:   waHistorySync.HistorySync_FULL.Enum(),
		ChunkOrder: proto.Uint32(1),
		Progress:   proto.Uint32(100),
		Conversations: []*waHistorySync.Conversation{
			{ID: proto.String("1@s.whatsapp.net"), Messages: []*waHistorySync.HistorySyncMsg{{}, {}}},
		},
	}
	raw, _ := proto.Marshal(hs)
	// An unknown field 99 (varint 1), as a newer phone might add.
	raw = append(raw, 0x98, 0x06, 0x01)
	var parsed waHistorySync.HistorySync
	if err := proto.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	got := describeHistorySyncBlob(&waE2E.HistorySyncNotification{FileLength: proto.Uint64(42)}, &parsed, 10, len(raw))
	for _, want := range []string{"type=FULL", "declared_len=42", "unknown_bytes=3", "conversations=1", "messages=2", "conversations[1]", "syncType"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing %q", got, want)
		}
	}
	if strings.Contains(got, "1@s.whatsapp.net") {
		t.Error("the summary must not include content")
	}
}
