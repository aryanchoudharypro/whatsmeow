package whatsmeow

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
)

// describeHistorySyncBlob summarizes a history sync blob for the log without
// any of its content: its sizes, which top-level fields it carries (with a
// count for lists), how many bytes of it the protobuf definitions don't
// recognize, and the notification's own metadata. It's what tells an empty
// chunk apart from one whose history arrived in a field this version can't
// read.
func describeHistorySyncBlob(notif *waE2E.HistorySyncNotification, hs *waHistorySync.HistorySync, compressedLen, rawLen int) string {
	var fields []string
	hs.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			fields = append(fields, fmt.Sprintf("%s[%d]", fd.Name(), v.List().Len()))
		case fd.IsMap():
			fields = append(fields, fmt.Sprintf("%s{%d}", fd.Name(), v.Map().Len()))
		default:
			fields = append(fields, string(fd.Name()))
		}
		return true
	})
	slices.Sort(fields)
	var messages int
	for _, conv := range hs.GetConversations() {
		messages += len(conv.GetMessages())
	}
	return fmt.Sprintf("type=%s chunk=%d progress=%d declared_len=%d compressed=%d decompressed=%d unknown_bytes=%d conversations=%d messages=%d fields=[%s] notif{oldest_ts=%d session=%t on_demand_meta=%t enc_handle=%t access_status=%t}",
		hs.GetSyncType(), hs.GetChunkOrder(), hs.GetProgress(),
		notif.GetFileLength(), compressedLen, rawLen, len(hs.ProtoReflect().GetUnknown()),
		len(hs.GetConversations()), messages, strings.Join(fields, " "),
		notif.GetOldestMsgInChunkTimestampSec(), notif.GetPeerDataRequestSessionID() != "",
		notif.GetFullHistorySyncOnDemandRequestMetadata() != nil, notif.GetEncHandle() != "",
		notif.GetMessageAccessStatus() != nil)
}
