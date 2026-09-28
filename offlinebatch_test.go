package whatsmeow

import (
	"context"
	"testing"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestOfflineBatchRequestShape(t *testing.T) {
	node := buildOfflineBatchRequest(200)
	children, ok := node.Content.([]waBinary.Node)
	if node.Tag != "ib" || !ok || len(children) != 1 || children[0].Tag != "offline_batch" ||
		children[0].AttrGetter().Int("count") != 200 {
		t.Fatalf("got %v", node)
	}
}

func TestOfflineBatchPullLoop(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &cli.offlineBatch

	cli.startOfflineBatches(ctx, 500)
	if !st.armed.Load() || !st.inflight.Load() {
		t.Fatal("not armed with the first batch in flight")
	}
	// The first stanza of the answer claims the continuation...
	cli.noteOfflineStanza(ctx)
	if st.inflight.Load() {
		t.Fatal("first arrival didn't claim the continuation")
	}
	// ...and later stanzas of the same batch don't schedule another.
	cli.noteOfflineStanza(ctx)
	if st.inflight.Load() || st.processed.Load() != 2 {
		t.Fatalf("inflight %v processed %d", st.inflight.Load(), st.processed.Load())
	}
	// Once the next request has gone out, its first stanza can claim again.
	deadline := time.Now().Add(2 * time.Second)
	for !st.inflight.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !st.inflight.Load() {
		t.Fatal("continuation never re-armed the next batch")
	}
	// Nothing is scheduled once the backlog is all here.
	st.processed.Store(499)
	cli.noteOfflineStanza(ctx)
	if !st.inflight.Load() {
		t.Fatal("a continuation was claimed with nothing pending")
	}
	cli.stopOfflineBatches()
	cli.noteOfflineStanza(ctx)
	if st.processed.Load() != 500 {
		t.Fatal("a disarmed loop kept counting")
	}
}
