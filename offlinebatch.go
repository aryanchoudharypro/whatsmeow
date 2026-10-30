package whatsmeow

import (
	"context"
	"math"
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

// offlineBatchState holds the drain of the current connection. Each announced
// backlog gets a fresh offlineBatchRun, so a send or watchdog left over from
// an earlier connection can only touch the run it started with, never the
// inflight flag or activity counter of the one that replaced it.
type offlineBatchState struct {
	run atomic.Pointer[offlineBatchRun]
}

type offlineBatchRun struct {
	armed atomic.Bool
	// inflight is true between sending a batch request and the first stanza
	// of its answer; that stanza's arrival schedules the next request.
	inflight atomic.Bool
	// activity is bumped by every offline stanza and every request sent, so
	// the watchdog's window always starts after the latest of them.
	activity  atomic.Uint64
	total     int64
	processed atomic.Int64
}

// current returns the armed run, or nil when no drain is going on.
func (st *offlineBatchState) current() *offlineBatchRun {
	if run := st.run.Load(); run != nil && run.armed.Load() {
		return run
	}
	return nil
}

func (st *offlineBatchState) isCurrent(run *offlineBatchRun) bool {
	return st.run.Load() == run && run.armed.Load()
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
	run := &offlineBatchRun{total: int64(total)}
	run.inflight.Store(true)
	run.armed.Store(true)
	if old := cli.offlineBatch.run.Swap(run); old != nil {
		old.armed.Store(false)
	}
	cli.Log.Infof("Offline backlog of %d stanzas announced, requesting the first batch of %d", total, offlineBatchSize)
	go cli.offlineBatchWatchdog(ctx, run)
	cli.sendOfflineBatch(ctx)
}

// stopOfflineBatches disarms the pull loop (backlog done, or connection gone).
func (cli *Client) stopOfflineBatches() {
	if run := cli.offlineBatch.run.Load(); run != nil {
		run.armed.Store(false)
	}
}

// noteOfflineStanza is called for every stanza carrying the offline attribute.
func (cli *Client) noteOfflineStanza(ctx context.Context) {
	st := &cli.offlineBatch
	run := st.current()
	if run == nil {
		return
	}
	run.activity.Add(1)
	pending := run.total - run.processed.Add(1)
	if pending <= 0 {
		// Nothing more is owed; the <ib><offline> end marker completes it.
		return
	}
	if !run.inflight.CompareAndSwap(true, false) {
		return
	}
	go func() {
		time.Sleep(offlineBatchDebounce)
		if !st.isCurrent(run) || cli.offlineSyncDone.Load() {
			return
		}
		// Published before the send rather than after it: the answer can
		// start arriving while sendNode is still returning, and its first
		// stanza has to find the batch in flight to schedule the next one.
		// Setting it afterwards would also undo a claim that stanza already
		// made and let a second request go out for the same batch. The
		// activity bump opens a fresh watchdog window for the request.
		run.activity.Add(1)
		run.inflight.Store(true)
		cli.sendOfflineBatch(ctx)
	}()
}
func (cli *Client) sendOfflineBatch(ctx context.Context) {
	// Acknowledge what the previous batch delivered before asking for more.
	cli.flushOfflineReceipts(ctx)
	if err := cli.sendNode(ctx, buildOfflineBatchRequest(offlineBatchSize)); err != nil {
		cli.Log.Warnf("Failed to request offline batch: %v", err)
		return
	}
	cli.Log.Debugf("Requested offline batch of %d", offlineBatchSize)
}

// offlineBatchWatchdog completes a drain the server has stopped feeding.
func (cli *Client) offlineBatchWatchdog(ctx context.Context, run *offlineBatchRun) {
	st := &cli.offlineBatch
	seen := run.activity.Load()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(offlineStanzaTimeout):
		}
		if !st.isCurrent(run) || cli.offlineSyncDone.Load() {
			return
		}
		if current := run.activity.Load(); current != seen {
			seen = current
			continue
		}
		// A stanza or request landing between the read above and this
		// decision fails the swap, so a drain still being fed is never
		// completed behind the server's back.
		if !run.activity.CompareAndSwap(seen, math.MaxUint64) {
			seen = run.activity.Load()
			continue
		}
		if !run.armed.CompareAndSwap(true, false) {
			return
		}
		processed := int(run.processed.Load())
		cli.Log.Warnf("No offline stanza for %s; completing the offline sync at %d stanzas", offlineStanzaTimeout, processed)
		cli.offlineSyncDone.Store(true)
		cli.flushOfflineReceipts(ctx)
		cli.dispatchEvent(&events.OfflineSyncCompleted{Count: processed})
		return
	}
}
