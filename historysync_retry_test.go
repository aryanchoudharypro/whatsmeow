package whatsmeow

import (
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestHistorySyncChunkRetryRequestShape(t *testing.T) {
	msg := BuildHistorySyncChunkRetryRequest(waE2E.HistorySyncType_RECENT, 3, "NOTIF", true)
	pdo := msg.GetProtocolMessage().GetPeerDataOperationRequestMessage()
	req := pdo.GetHistorySyncChunkRetryRequest()
	if pdo.GetPeerDataOperationRequestType() != waE2E.PeerDataOperationRequestType_HISTORY_SYNC_CHUNK_RETRY ||
		req.GetSyncType() != waE2E.HistorySyncType_RECENT || req.GetChunkOrder() != 3 ||
		req.GetChunkNotificationID() != "NOTIF" || !req.GetRegenerateChunk() {
		t.Fatalf("got %v", msg)
	}
}

func TestFullHistorySyncOnDemandRequestShape(t *testing.T) {
	from := time.UnixMilli(1700000000000)
	msg := BuildFullHistorySyncOnDemandRequest("REQ", from, 3650, nil)
	pdo := msg.GetProtocolMessage().GetPeerDataOperationRequestMessage()
	req := pdo.GetFullHistorySyncOnDemandRequest()
	if pdo.GetPeerDataOperationRequestType() != waE2E.PeerDataOperationRequestType_FULL_HISTORY_SYNC_ON_DEMAND ||
		req.GetRequestMetadata().GetRequestID() != "REQ" ||
		req.GetFullHistorySyncOnDemandConfig().GetHistoryFromTimestamp() != 1700000000000 ||
		req.GetFullHistorySyncOnDemandConfig().GetHistoryDurationDays() != 3650 ||
		req.GetHistorySyncConfig() == nil || req.GetHistorySyncConfig() == store.DeviceProps.GetHistorySyncConfig() {
		t.Fatalf("got %v", msg)
	}
}

func TestHistorySyncChunkGone(t *testing.T) {
	if !historySyncChunkGone(fmt.Errorf("wrapped: %w", ErrMediaDownloadFailedWith410)) || !historySyncChunkGone(ErrInvalidMediaHMAC) {
		t.Fatal("a gone or broken blob wasn't recognised")
	}
	if historySyncChunkGone(fmt.Errorf("connection reset")) {
		t.Fatal("a network error counted as a gone blob")
	}
}

// A slow handler mustn't hold the handler list lock: adding a handler while
// it runs used to block every other dispatch until it returned.
func TestDispatchDoesNotHoldHandlerLock(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	release := make(chan struct{})
	started := make(chan struct{})
	cli.AddEventHandler(func(evt any) {
		if evt == "slow" {
			close(started)
			<-release
		}
	})
	go cli.dispatchEvent("slow")
	<-started
	done := make(chan struct{})
	go func() {
		cli.AddEventHandler(func(any) {})
		cli.dispatchEvent("fast")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch blocked behind a slow handler")
	}
	close(release)
}
