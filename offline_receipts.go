// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"strconv"
	"sync"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// Delivery receipts for an offline backlog are sent aggregated, as WA Web
// does (sendAggregateOfflineReceipts): one <receipt> per chat and author with
// the other message IDs in a <list>, instead of one stanza per message.
const (
	// maxReceiptIDsPerStanza is WA Web's limit on IDs in one receipt.
	maxReceiptIDsPerStanza = 256
	// offlineReceiptFlushDelay bounds how long a buffered receipt waits.
	// Until it's sent the server treats the message as unacknowledged.
	offlineReceiptFlushDelay = 2 * time.Second
)

type offlineReceiptGroup struct {
	attrs waBinary.Attrs
	ids   []string
}

type offlineReceiptBuffer struct {
	lock   sync.Mutex
	order  []string
	groups map[string]*offlineReceiptGroup
	timer  *time.Timer
}

// offlineReceiptKey identifies the receipts that can share one stanza: same
// destination, author, recipient and type.
func offlineReceiptKey(attrs waBinary.Attrs) string {
	str := func(key string) string {
		switch v := attrs[key].(type) {
		case types.JID:
			return v.String()
		case string:
			return v
		case nil:
			return ""
		default:
			return "?"
		}
	}
	return str("to") + "|" + str("participant") + "|" + str("recipient") + "|" + str("type")
}

// bufferOfflineReceipt queues a delivery receipt for a message from the
// offline backlog. attrs is the receipt without its id. Peer messages, and
// anything once the backlog is done, aren't buffered.
func (cli *Client) bufferOfflineReceipt(ctx context.Context, id types.MessageID, attrs waBinary.Attrs) bool {
	if cli.offlineSyncDone.Load() {
		return false
	}
	buf := &cli.offlineReceipts
	key := offlineReceiptKey(attrs)
	buf.lock.Lock()
	if buf.groups == nil {
		buf.groups = make(map[string]*offlineReceiptGroup)
	}
	group, ok := buf.groups[key]
	if !ok {
		group = &offlineReceiptGroup{attrs: attrs}
		buf.groups[key] = group
		buf.order = append(buf.order, key)
	}
	group.ids = append(group.ids, string(id))
	full := len(group.ids) >= maxReceiptIDsPerStanza
	if buf.timer == nil && !full {
		buf.timer = time.AfterFunc(offlineReceiptFlushDelay, func() {
			cli.flushOfflineReceipts(context.WithoutCancel(ctx))
		})
	}
	buf.lock.Unlock()
	if full {
		cli.flushOfflineReceipts(ctx)
	}
	return true
}

// flushOfflineReceipts sends every buffered delivery receipt, aggregated.
func (cli *Client) flushOfflineReceipts(ctx context.Context) {
	buf := &cli.offlineReceipts
	buf.lock.Lock()
	if buf.timer != nil {
		buf.timer.Stop()
		buf.timer = nil
	}
	order, groups := buf.order, buf.groups
	buf.order, buf.groups = nil, nil
	buf.lock.Unlock()
	if len(order) == 0 {
		return
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	total := 0
	for _, key := range order {
		group := groups[key]
		total += len(group.ids)
		for start := 0; start < len(group.ids); start += maxReceiptIDsPerStanza {
			chunk := group.ids[start:min(start+maxReceiptIDsPerStanza, len(group.ids))]
			if err := cli.sendNode(ctx, buildAggregateReceipt(group.attrs, chunk, now)); err != nil {
				cli.Log.Warnf("Failed to send aggregate delivery receipt for %d messages to %v: %v", len(chunk), group.attrs["to"], err)
			}
		}
	}
	cli.Log.Debugf("Sent %d offline delivery receipts in %d groups", total, len(order))
}

// buildAggregateReceipt builds one receipt for several messages: the first
// ID in the id attribute, the rest as <list><item id/></list>.
func buildAggregateReceipt(base waBinary.Attrs, ids []string, timestamp string) waBinary.Node {
	attrs := make(waBinary.Attrs, len(base)+2)
	for k, v := range base {
		attrs[k] = v
	}
	attrs["id"] = ids[0]
	attrs["t"] = timestamp
	node := waBinary.Node{Tag: "receipt", Attrs: attrs}
	if len(ids) > 1 {
		items := make([]waBinary.Node, len(ids)-1)
		for i, id := range ids[1:] {
			items[i] = waBinary.Node{Tag: "item", Attrs: waBinary.Attrs{"id": id}}
		}
		node.Content = []waBinary.Node{{Tag: "list", Content: items}}
	}
	return node
}
