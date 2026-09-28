package whatsmeow

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestHistorySyncDownloadsDispatchInOrder(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	cli.BackgroundEventCtx = context.Background()

	var running, peak atomic.Int32
	cli.historySyncDownloader = func(_ context.Context, notif *waE2E.HistorySyncNotification, _ bool) (*waHistorySync.HistorySync, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		// Earlier chunks take longer, so finishing order is the reverse of
		// arrival order.
		time.Sleep(time.Duration(10-notif.GetChunkOrder()) * 15 * time.Millisecond)
		running.Add(-1)
		return &waHistorySync.HistorySync{ChunkOrder: proto.Uint32(notif.GetChunkOrder())}, nil
	}

	const chunks = 6
	var mu sync.Mutex
	var order []uint32
	done := make(chan struct{})
	cli.AddEventHandler(func(evt any) {
		if hs, ok := evt.(*events.HistorySync); ok {
			mu.Lock()
			order = append(order, hs.Data.GetChunkOrder())
			if len(order) == chunks {
				close(done)
			}
			mu.Unlock()
		}
	})

	for i := range chunks {
		cli.historySyncNotifications <- &waE2E.HistorySyncNotification{ChunkOrder: proto.Uint32(uint32(i)), DirectPath: proto.String("/v/t62/history")}
	}
	cli.historySyncHandlerStarted.Store(true)
	go cli.handleHistorySyncNotificationLoop()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("not every chunk was dispatched")
	}
	for i, got := range order {
		if got != uint32(i) {
			t.Fatalf("dispatch order %v, want 0..%d", order, chunks-1)
		}
	}
	if p := peak.Load(); p < min(2, historySyncParallelDownloads) || p > historySyncParallelDownloads {
		t.Fatalf("peak concurrent downloads %d, want at most %d", p, historySyncParallelDownloads)
	}
}

func TestHistorySyncFailedDownloadDoesNotBlockLaterChunks(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	cli.BackgroundEventCtx = context.Background()
	cli.historySyncDownloader = func(_ context.Context, notif *waE2E.HistorySyncNotification, _ bool) (*waHistorySync.HistorySync, error) {
		if notif.GetChunkOrder() == 0 {
			panic("broken blob")
		}
		return &waHistorySync.HistorySync{ChunkOrder: proto.Uint32(notif.GetChunkOrder())}, nil
	}
	got := make(chan uint32, 2)
	cli.AddEventHandler(func(evt any) {
		if hs, ok := evt.(*events.HistorySync); ok {
			got <- hs.Data.GetChunkOrder()
		}
	})
	cli.historySyncNotifications <- &waE2E.HistorySyncNotification{ChunkOrder: proto.Uint32(0), DirectPath: proto.String("/v/t62/history")}
	cli.historySyncNotifications <- &waE2E.HistorySyncNotification{ChunkOrder: proto.Uint32(1), DirectPath: proto.String("/v/t62/history")}
	cli.historySyncHandlerStarted.Store(true)
	go cli.handleHistorySyncNotificationLoop()
	select {
	case c := <-got:
		if c != 1 {
			t.Fatalf("dispatched chunk %d, want 1", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a failed download held up the next chunk")
	}
}

func TestHistorySyncStatusNotificationIsNotDownloaded(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	cli.BackgroundEventCtx = context.Background()
	cli.historySyncDownloader = func(context.Context, *waE2E.HistorySyncNotification, bool) (*waHistorySync.HistorySync, error) {
		t.Error("a status-only notification was downloaded")
		return nil, nil
	}
	got := make(chan *events.HistorySyncStatus, 1)
	cli.AddEventHandler(func(evt any) {
		if st, ok := evt.(*events.HistorySyncStatus); ok {
			got <- st
		}
	})
	cli.historySyncNotifications <- &waE2E.HistorySyncNotification{
		SyncType:            waE2E.HistorySyncType_MESSAGE_ACCESS_STATUS.Enum(),
		MessageAccessStatus: &waE2E.HistorySyncMessageAccessStatus{CompleteAccessGranted: proto.Bool(false)},
	}
	cli.historySyncHandlerStarted.Store(true)
	go cli.handleHistorySyncNotificationLoop()
	select {
	case st := <-got:
		if st.Notification.GetSyncType() != waE2E.HistorySyncType_MESSAGE_ACCESS_STATUS ||
			st.Notification.GetMessageAccessStatus().GetCompleteAccessGranted() {
			t.Fatalf("status %v", st.Notification)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no status event")
	}
}
