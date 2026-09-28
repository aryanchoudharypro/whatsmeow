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
// dispatched one at a time, in the order the notifications arrived.
const historySyncParallelDownloads = 3

// historySyncDownload is one notification's blob being fetched and parsed.
type historySyncDownload struct {
	notif *waE2E.HistorySyncNotification
	done  chan struct{}
	blob  *waHistorySync.HistorySync
	err   error
}

func (cli *Client) startHistorySyncDownload(ctx context.Context, notif *waE2E.HistorySyncNotification) *historySyncDownload {
	dl := &historySyncDownload{notif: notif, done: make(chan struct{})}
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

// finishHistorySyncDownload dispatches a fetched chunk and then deletes its
// blob from the media server. The delete runs on its own so the next chunk
// doesn't wait on that round trip.
func (cli *Client) finishHistorySyncDownload(ctx context.Context, dl *historySyncDownload) {
	if dl.err != nil {
		cli.Log.Errorf("Failed to download history sync: %v", dl.err)
		return
	}
	cli.dispatchEvent(&events.HistorySync{Data: dl.blob, Notification: dl.notif})
	notif := dl.notif
	go func() {
		err := cli.DeleteMedia(ctx, MediaHistory, notif.GetDirectPath(), notif.GetFileEncSHA256(), notif.GetEncHandle())
		if err != nil {
			cli.Log.Warnf("Failed to delete history sync media from server: %v", err)
		}
	}()
}
