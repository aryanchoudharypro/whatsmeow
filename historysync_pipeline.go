package whatsmeow

import (
	"context"
	"fmt"
	"runtime/debug"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types/events"
)

// historySyncParallelDownloads is how many history sync blobs are fetched at
// once when the phone has sent several notifications. They are still
// dispatched one at a time, in the order the notifications arrived. Kept at
// 1 (one download at a time, as upstream does) while a sync that stops early
// is being narrowed down.
const historySyncParallelDownloads = 1

// historySyncDownload is one notification's blob being fetched and parsed.
type historySyncDownload struct {
	notif *waE2E.HistorySyncNotification
	done  chan struct{}
	blob  *waHistorySync.HistorySync
	err   error
	// statusOnly is a notification with nothing to download.
	statusOnly bool
}

// historySyncHasBlob reports whether a notification points at history to
// download (or carries it inline), as opposed to only a status.
func historySyncHasBlob(notif *waE2E.HistorySyncNotification) bool {
	return notif.GetDirectPath() != "" || len(notif.GetInitialHistBootstrapInlinePayload()) > 0
}

func (cli *Client) startHistorySyncDownload(ctx context.Context, notif *waE2E.HistorySyncNotification) *historySyncDownload {
	dl := &historySyncDownload{notif: notif, done: make(chan struct{})}
	if !historySyncHasBlob(notif) {
		dl.statusOnly = true
		close(dl.done)
		return dl
	}
	download := cli.DownloadHistorySync
	if cli.historySyncDownloader != nil {
		download = cli.historySyncDownloader
	}
	go func() {
		defer close(dl.done)
		defer func() {
			if r := recover(); r != nil {
				cli.Log.Errorf("History sync download panicked: %v\n%s", r, debug.Stack())
				dl.blob, dl.err = nil, fmt.Errorf("panicked: %v", r)
			}
		}()
		dl.blob, dl.err = download(ctx, notif, false)
	}()
	return dl
}

// finishHistorySyncDownload dispatches a fetched chunk (or a status-only
// notification) and then deletes the chunk's blob from the media server.
func (cli *Client) finishHistorySyncDownload(ctx context.Context, dl *historySyncDownload) {
	if dl.statusOnly {
		cli.Log.Infof("Received history sync status (type %s, chunk %d, progress %d, complete access %v)",
			dl.notif.GetSyncType(), dl.notif.GetChunkOrder(), dl.notif.GetProgress(),
			dl.notif.GetMessageAccessStatus().GetCompleteAccessGranted())
		cli.dispatchEvent(&events.HistorySyncStatus{Notification: dl.notif})
		return
	}
	if dl.err != nil {
		cli.Log.Errorf("Failed to download history sync: %v", dl.err)
		return
	}
	cli.dispatchEvent(&events.HistorySync{Data: dl.blob, Notification: dl.notif})
	notif := dl.notif
	err := cli.DeleteMedia(ctx, MediaHistory, notif.GetDirectPath(), notif.GetFileEncSHA256(), notif.GetEncHandle())
	if err != nil {
		cli.Log.Warnf("Failed to delete history sync media from server: %v", err)
	}
}
