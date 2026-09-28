package whatsmeow

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types/events"
)

// Pull-based offline backlog. With pull=true in the login payload the server
// announces a backlog in <ib><offline_preview count="N"/>, delivers a short
// primer and then waits for <ib><offline_batch count="200"/> requests before
// sending more - WA Web's WAWebOfflineHandler. Without them the rest of the
// backlog is never delivered on that connection.
const (
	offlineBatchSize     = 200
	offlineBatchDebounce = 100 * time.Millisecond
	// A drain the server stops feeding is completed by the client, so the
	// OfflineSyncCompleted event is never stranded (WA Web's
	// OFFLINE_STANZA_TIMEOUT_MS).
	offlineStanzaTimeout = 60 * time.Second
)

type offlineBatchState struct {
	armed      atomic.Bool
	generation atomic.Uint64
	// inflight is true between sending a batch request and the first stanza
	// of its answer; that stanza's arrival schedules the next request.
	inflight  atomic.Bool
	activity  atomic.Uint64
	total     atomic.Int64
	processed atomic.Int64
}

func buildOfflineBatchRequest(count int) waBinary.Node {
	return waBinary.Node{
		Tag: "ib",
		Content: []waBinary.Node{{
			Tag:   "offline_batch",
			Attrs: waBinary.Attrs{"count": strconv.Itoa(count)},
		}},
	}
}

// startOfflineBatches arms the pull loop for an announced backlog of total
// stanzas and requests the first batch.
func (cli *Client) startOfflineBatches(ctx context.Context, total int) {
	st := &cli.offlineBatch
	gen := st.generation.Add(1)
	st.total.Store(int64(total))
	st.processed.Store(0)
	st.activity.Store(0)
	st.inflight.Store(true)
	st.armed.Store(true)
	cli.Log.Infof("Offline backlog of %d stanzas announced, requesting the first batch of %d", total, offlineBatchSize)
	go cli.offlineBatchWatchdog(ctx, gen)
	cli.sendOfflineBatch(ctx)
}

// stopOfflineBatches disarms the pull loop (backlog done, or connection gone).
func (cli *Client) stopOfflineBatches() {
	cli.offlineBatch.armed.Store(false)
}

// noteOfflineStanza is called for every stanza carrying the offline attribute.
func (cli *Client) noteOfflineStanza(ctx context.Context) {
	st := &cli.offlineBatch
	if !st.armed.Load() {
		return
	}
	st.activity.Add(1)
	pending := st.total.Load() - st.processed.Add(1)
	if pending <= 0 {
		// Nothing more is owed; the <ib><offline> end marker completes it.
		return
	}
	if !st.inflight.CompareAndSwap(true, false) {
		return
	}
	gen := st.generation.Load()
	go func() {
		time.Sleep(offlineBatchDebounce)
		if !st.armed.Load() || st.generation.Load() != gen || cli.offlineSyncDone.Load() {
			return
		}
		cli.sendOfflineBatch(ctx)
		if st.armed.Load() && st.generation.Load() == gen {
			st.inflight.Store(true)
		}
	}()
}

func (cli *Client) sendOfflineBatch(ctx context.Context) {
	if err := cli.sendNode(ctx, buildOfflineBatchRequest(offlineBatchSize)); err != nil {
		cli.Log.Warnf("Failed to request offline batch: %v", err)
		return
	}
	cli.Log.Debugf("Requested offline batch of %d", offlineBatchSize)
}

// offlineBatchWatchdog completes a drain the server has stopped feeding.
func (cli *Client) offlineBatchWatchdog(ctx context.Context, gen uint64) {
	st := &cli.offlineBatch
	seen := st.activity.Load()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(offlineStanzaTimeout):
		}
		if !st.armed.Load() || st.generation.Load() != gen || cli.offlineSyncDone.Load() {
			return
		}
		if current := st.activity.Load(); current != seen {
			seen = current
			continue
		}
		if !st.armed.CompareAndSwap(true, false) {
			return
		}
		processed := int(st.processed.Load())
		cli.Log.Warnf("No offline stanza for %s; completing the offline sync at %d stanzas", offlineStanzaTimeout, processed)
		cli.offlineSyncDone.Store(true)
		cli.dispatchEvent(&events.OfflineSyncCompleted{Count: processed})
		return
	}
}
