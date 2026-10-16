// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"fmt"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestOfflineReceiptsGroupByChatAndAuthor(t *testing.T) {
	cli := &Client{Log: waLog.Noop}
	ctx := context.Background()
	group := types.NewJID("120363000000000001", types.GroupServer)
	alice := types.NewADJID("15550000001", 0, 0)
	bob := types.NewADJID("15550000002", 0, 3)
	dm := types.NewADJID("15550000003", 0, 0)
	add := func(id string, attrs waBinary.Attrs) {
		if !cli.bufferOfflineReceipt(ctx, types.MessageID(id), attrs) {
			t.Fatal("not buffered")
		}
	}
	for i := range 3 {
		add(fmt.Sprintf("A%d", i), waBinary.Attrs{"to": group, "participant": alice})
		add(fmt.Sprintf("B%d", i), waBinary.Attrs{"to": group, "participant": bob})
		add(fmt.Sprintf("D%d", i), waBinary.Attrs{"to": dm})
	}
	add("I0", waBinary.Attrs{"to": dm, "type": "inactive"})
	buf := &cli.offlineReceipts
	if len(buf.order) != 4 {
		t.Fatalf("got %d groups, want 4", len(buf.order))
	}
	first := buf.groups[buf.order[0]]
	if fmt.Sprint(first.ids) != "[A0 A1 A2]" {
		t.Fatal(first.ids)
	}
	node := buildAggregateReceipt(first.attrs, first.ids, "1700000000")
	if node.Attrs["id"] != "A0" || node.Attrs["participant"] != alice || node.Attrs["t"] != "1700000000" {
		t.Fatalf("bad attrs %v", node.Attrs)
	}
	listNode := node.GetChildByTag("list")
	list := listNode.GetChildren()
	if len(list) != 2 || list[0].Attrs["id"] != "A1" || list[1].Attrs["id"] != "A2" {
		t.Fatalf("bad list %v", list)
	}
	if single := buildAggregateReceipt(first.attrs, []string{"X"}, "1"); single.Content != nil {
		t.Fatal("single receipt must have no list")
	}
	// Without a connection a flush keeps everything for the next one.
	cli.flushOfflineReceipts(ctx)
	if len(buf.order) != 4 || len(buf.groups[buf.order[0]].ids) != 3 || buf.timer != nil {
		t.Fatal("receipts lost while disconnected")
	}
	buf.reset()
	if len(buf.order) != 0 {
		t.Fatal("not reset")
	}
	cli.offlineSyncDone.Store(true)
	if cli.bufferOfflineReceipt(ctx, "late", waBinary.Attrs{"to": dm}) {
		t.Fatal("buffered after the backlog was done")
	}
}
