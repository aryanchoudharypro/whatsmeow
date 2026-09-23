// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"strconv"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Dirty bit types the server can raise in an <ib><dirty> stanza.
const (
	DirtyTypeAccountSync        = "account_sync"
	DirtyTypeGroups             = "groups"
	DirtyTypeSyncdAppState      = "syncd_app_state"
	DirtyTypeNewsletterMetadata = "newsletter_metadata"
)

// offlineSyncWaitLimit bounds how long a dirty bit waits for offline delivery
// to finish before being handled anyway.
const offlineSyncWaitLimit = 2 * time.Minute

// handleDirtyBit handles the server saying some of our state is stale, the
// way WhatsApp Web's WAWebHandleDirtyBits does: group and newsletter bits
// wait until offline delivery is over, then the bit is marked clean, and a
// stale app state is synced again. The server only raises a bit once, so a
// client that never answers keeps the stale data.
func (cli *Client) handleDirtyBit(dirtyType, rawTimestamp string) {
	if dirtyType == "" {
		cli.Log.Warnf("Dirty notification without a type")
		return
	}
	var ts time.Time
	if rawTimestamp != "" {
		unix, err := strconv.ParseInt(rawTimestamp, 10, 64)
		if err != nil {
			cli.Log.Warnf("Dirty notification with invalid timestamp %q", rawTimestamp)
			return
		}
		ts = time.Unix(unix, 0)
	}
	ctx := cli.BackgroundEventCtx
	cli.dispatchEvent(&events.DirtyState{Type: dirtyType, Timestamp: ts})

	if dirtyType == DirtyTypeGroups || dirtyType == DirtyTypeNewsletterMetadata {
		if !cli.waitForOfflineSync(offlineSyncWaitLimit) {
			return
		}
	}
	if err := cli.markClean(ctx, dirtyType, ts); err != nil {
		cli.Log.Warnf("Failed to mark %s dirty bit as clean: %v", dirtyType, err)
	}
	switch dirtyType {
	case DirtyTypeGroups:
		// Every group's member list may be out of date; the next send to
		// each one fetches it again.
		cli.groupCacheLock.Lock()
		clear(cli.groupCache)
		cli.groupCacheLock.Unlock()
	case DirtyTypeSyncdAppState:
		for _, name := range appstate.AllPatchNames {
			if err := cli.FetchAppState(ctx, name, false, false); err != nil {
				cli.Log.Warnf("Failed to sync %s after a dirty notification: %v", name, err)
			}
		}
	}
}

// waitForOfflineSync waits until the server has delivered everything that
// arrived while we were offline, or the limit passes. It returns false if the
// connection went away meanwhile.
func (cli *Client) waitForOfflineSync(limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for !cli.offlineSyncDone.Load() && time.Now().Before(deadline) {
		if !cli.IsConnected() {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	return cli.IsConnected()
}

// markClean tells the server a dirty bit was handled. The timestamp is only
// sent when the server gave one.
func (cli *Client) markClean(ctx context.Context, dirtyType string, ts time.Time) error {
	attrs := waBinary.Attrs{"type": dirtyType}
	if !ts.IsZero() {
		attrs["timestamp"] = ts.Unix()
	}
	_, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "urn:xmpp:whatsapp:dirty",
		Type:      iqSet,
		To:        types.ServerJID,
		Content:   []waBinary.Node{{Tag: "clean", Attrs: attrs}},
	})
	return err
}
