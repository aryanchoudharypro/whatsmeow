package whatsmeow

import (
	"context"
	"fmt"
	"runtime/debug"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// historySyncParallelDownloads is how many history sync blobs are fetched at
// once when the phone has sent several notifications. They are still
// dispatched one at a time, in the order the notifications arrived. Kept at
// 1 (one download at a time, as upstream does) while a sync that stops early
// is being narrowed down.
const historySyncParallelDownloads = 1

// historySyncRequest is a history sync notification queued for download.
type historySyncRequest struct {
	notif *waE2E.HistorySyncNotification
	// receiptID is the notification's message ID when its receipt is still
	// owed (see Client.AckHistorySyncAfterDispatch), empty if it was already
	// sent on arrival.
	receiptID types.MessageID
}

// historySyncDownload is one notification's blob being fetched and parsed.
type historySyncDownload struct {
	notif     *waE2E.HistorySyncNotification
	receiptID types.MessageID
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

func (cli *Client) startHistorySyncDownload(ctx context.Context, req historySyncRequest) *historySyncDownload {
	notif := req.notif
	dl := &historySyncDownload{notif: notif, receiptID: req.receiptID, done: make(chan struct{})}
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

// sendDeferredHistorySyncReceipt acknowledges a chunk whose receipt was held
// back until it had been handled.
func (cli *Client) sendDeferredHistorySyncReceipt(ctx context.Context, dl *historySyncDownload) {
	if dl.receiptID == "" {
		return
	}
	var err error
	if cli.historySyncReceiptSender != nil {
		err = cli.historySyncReceiptSender(ctx, dl.receiptID)
	} else {
		err = cli.SendProtocolMessageReceipt(ctx, dl.receiptID, types.ReceiptTypeHistorySync)
	}
	if err != nil {
		cli.Log.Warnf("Failed to send acknowledgement for protocol message %s: %v", dl.receiptID, err)
	}
}

// finishHistorySyncDownload dispatches a fetched chunk (or a status-only
// notification) and then deletes the chunk's blob from the media server.
func (cli *Client) finishHistorySyncDownload(ctx context.Context, dl *historySyncDownload) {
	if dl.statusOnly {
		cli.Log.Infof("Received history sync status (type %s, chunk %d, progress %d, complete access %v)",
			dl.notif.GetSyncType(), dl.notif.GetChunkOrder(), dl.notif.GetProgress(),
			dl.notif.GetMessageAccessStatus().GetCompleteAccessGranted())
		cli.dispatchEvent(&events.HistorySyncStatus{Notification: dl.notif})
		// Nothing to download, so there is nothing a receipt could lose.
		cli.sendDeferredHistorySyncReceipt(ctx, dl)
		return
	}
	if dl.err != nil {
		if dl.receiptID != "" {
			cli.Log.Errorf("Failed to download history sync %s, withholding its receipt: %v", dl.receiptID, dl.err)
		} else {
			cli.Log.Errorf("Failed to download history sync: %v", dl.err)
		}
		return
	}
	if cli.dispatchEvent(&events.HistorySync{Data: dl.blob, Notification: dl.notif}) && dl.receiptID != "" {
		cli.Log.Errorf("History sync %s was not handled, withholding its receipt", dl.receiptID)
		return
	}
	cli.sendDeferredHistorySyncReceipt(ctx, dl)
	notif := dl.notif
	err := cli.DeleteMedia(ctx, MediaHistory, notif.GetDirectPath(), notif.GetFileEncSHA256(), notif.GetEncHandle())
	if err != nil {
		cli.Log.Warnf("Failed to delete history sync media from server: %v", err)
	}
}
