// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"encoding/json"
	"maps"
	"strconv"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Some of the server's feature flags ("AB props"), by config code, as
// WhatsApp Web names them.
const (
	ABPropChannelsPinAdminEnabled    = 29516 // channels_message_pin_admin_enabled
	ABPropChannelsPinFollowerEnabled = 29517 // channels_message_pin_follower_enabled
	ABPropSpamReportWithPrivacyToken = 4991  // enable_spam_report_iq_with_privacy_token
)

// ABProps are the server's feature flags for this account: config code to
// value. The official apps fetch them when they connect and hide what the
// server hasn't turned on.
type ABProps map[int]string

// Bool reads a flag that's on or off, with def when the server didn't send it.
// Like WA Web (WAWebABPropsParseConfigValue), "1", "True" and "true" are on.
func (props ABProps) Bool(code int, def bool) bool {
	value, ok := props[code]
	if !ok {
		return def
	}
	return ParseABPropBool(value)
}

// ParseABPropBool reads a bool flag's value the way WA Web does.
func ParseABPropBool(value string) bool {
	return value == "1" || value == "True" || value == "true"
}

// GetABProps fetches the server's feature flags, the way WhatsApp Web does
// (WASmaxOutAbPropsGetExperimentConfigRequest).
func (cli *Client) GetABProps(ctx context.Context) (ABProps, error) {
	resp, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "abt",
		Type:      iqGet,
		To:        types.ServerJID,
		Content: []waBinary.Node{{
			Tag:   "props",
			Attrs: waBinary.Attrs{"protocol": "1"},
		}},
	})
	if err != nil {
		return nil, err
	}
	return parseABProps(resp), nil
}

// CachedABProps returns the feature flags the client fetched when it last
// connected, without asking the server. It is empty until the first fetch
// after connecting has finished.
func (cli *Client) CachedABProps() ABProps {
	cli.abPropsLock.Lock()
	defer cli.abPropsLock.Unlock()
	return maps.Clone(cli.abPropsCache)
}

// refreshABProps fetches the feature flags after connecting when they may
// have changed. WhatsApp Web (WAWebHandleSuccess) only asks again when the
// refresh ID in the server's <success> differs from the one its stored flags
// came with, and its SYNC_AB_PROPS task asks again once the refresh interval
// the server gave has passed. The flags are kept in the store across
// restarts, like WA Web's local storage.
func (cli *Client) refreshABProps(ctx context.Context) {
	cli.abPropsLock.Lock()
	hash := cli.abPropsHash
	seeded := cli.abPropsCache != nil
	current := cli.abPropsAreCurrent(time.Now())
	// WA Web sends its hash only from the timed SYNC_AB_PROPS task. When
	// <success> names a new refresh ID, WAWebHandleSuccess asks without one
	// (shouldSendHash false) and gets the full set.
	newRefreshID := cli.abPropsServerRefreshID != 0 && cli.abPropsServerRefreshID != cli.abPropsRefreshID
	cli.abPropsLock.Unlock()
	if current {
		// The flags loaded from the store are still good.
		cli.announceStoredABProps()
		return
	}

	attrs := waBinary.Attrs{"protocol": "1"}
	// A delta is only what changed, so it is useless without the full set
	// it applies to.
	if seeded && hash != "" && !newRefreshID {
		attrs["hash"] = hash
	}
	resp, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "abt",
		Type:      iqGet,
		To:        types.ServerJID,
		Content:   []waBinary.Node{{Tag: "props", Attrs: attrs}},
	})
	if err != nil {
		cli.Log.Warnf("Failed to fetch AB props after connecting: %v", err)
		// Stale flags from the store beat none.
		cli.announceStoredABProps()
		return
	}
	cli.applyABProps(resp)
	cli.saveABProps(ctx)
	cli.abPropsLock.Lock()
	cli.abPropsAnnounced = true
	cli.abPropsLock.Unlock()
	cli.dispatchEvent(&events.ABPropsRefreshed{})
}

// announceStoredABProps sends ABPropsRefreshed once for flags loaded from
// the store, so the app reads them without a fetch.
func (cli *Client) announceStoredABProps() {
	cli.abPropsLock.Lock()
	announce := cli.abPropsCache != nil && !cli.abPropsAnnounced
	cli.abPropsAnnounced = true
	cli.abPropsLock.Unlock()
	if announce {
		cli.dispatchEvent(&events.ABPropsRefreshed{})
	}
}

// abPropsAreCurrent is whether the cached flags are the set the server's last
// <success> named, or it named none, and the refresh interval they came with
// hasn't run out. The caller holds abPropsLock.
func (cli *Client) abPropsAreCurrent(now time.Time) bool {
	if cli.abPropsCache == nil {
		return false
	}
	if !cli.abPropsSyncedAt.IsZero() && !now.Before(cli.abPropsSyncedAt.Add(cli.abPropsRefreshInterval())) {
		return false
	}
	return cli.abPropsServerRefreshID == 0 || cli.abPropsServerRefreshID == cli.abPropsRefreshID
}

// WA Web's limits on the refresh interval the server gives with the flags
// (WAWebABPropsLocalStorage), and its default when there is none.
const (
	abPropsMinRefresh     = 10 * time.Minute
	abPropsMaxRefresh     = 7 * 24 * time.Hour
	abPropsDefaultRefresh = 24 * time.Hour
)

// abPropsRefreshInterval is how long the cached flags stay good. The caller
// holds abPropsLock.
func (cli *Client) abPropsRefreshInterval() time.Duration {
	if cli.abPropsRefresh <= 0 {
		return abPropsDefaultRefresh
	}
	return min(max(cli.abPropsRefresh, abPropsMinRefresh), abPropsMaxRefresh)
}

// applyABProps takes a props response into the cache: a full set replaces
// it, a delta changes only the flags it names.
func (cli *Client) applyABProps(resp *waBinary.Node) {
	propsNode, ok := resp.GetOptionalChildByTag("props")
	if !ok {
		return
	}
	ag := propsNode.AttrGetter()
	props := parseABProps(resp)

	cli.abPropsLock.Lock()
	defer cli.abPropsLock.Unlock()
	delta := ag.OptionalBool("delta_update")
	newHash := ag.OptionalString("hash")
	switch {
	case !delta && newHash == "" && cli.abPropsCache != nil:
		// WA Web (WAWebAbPropsSyncJob) keeps its flags when a full answer
		// has no hash.
	case delta && cli.abPropsCache != nil:
		maps.Copy(cli.abPropsCache, props)
	default:
		cli.abPropsCache = props
	}
	cli.abPropsSyncedAt = time.Now()
	if refresh := ag.OptionalInt("refresh"); refresh > 0 {
		cli.abPropsRefresh = time.Duration(refresh) * time.Second
	}
	if newHash != "" {
		cli.abPropsHash = newHash
	}
	if refreshID := ag.OptionalInt("refresh_id"); refreshID != 0 {
		cli.abPropsRefreshID = refreshID
	} else if cli.abPropsServerRefreshID != 0 {
		// An answer without an ID is still the answer for the ID that was
		// current when it was asked for.
		cli.abPropsRefreshID = cli.abPropsServerRefreshID
	}
}

func parseABProps(resp *waBinary.Node) ABProps {
	props := ABProps{}
	propsNode, ok := resp.GetOptionalChildByTag("props")
	if !ok {
		return props
	}
	for _, prop := range propsNode.GetChildrenByTag("prop") {
		ag := prop.AttrGetter()
		code, err := strconv.Atoi(ag.OptionalString("config_code"))
		if err != nil || code <= 0 {
			// Sampling entries carry an event_code instead; they aren't flags.
			continue
		}
		props[code] = ag.OptionalString("config_value")
	}
	return props
}

// storedABProps is the cached flag set as it is kept in the store.
type storedABProps struct {
	Props     map[string]string `json:"props"`
	Hash      string            `json:"hash,omitempty"`
	RefreshID int               `json:"refresh_id,omitempty"`
	SyncedAt  int64             `json:"synced_at,omitempty"`
	Refresh   int64             `json:"refresh,omitempty"`
}

// loadABProps reads the flags kept from an earlier run into the cache, or
// clears it when there are none, so a new device never sees another's flags.
// The caller holds the connState lock.
func (cli *Client) loadABProps(ctx context.Context) {
	cli.abPropsLock.Lock()
	defer cli.abPropsLock.Unlock()
	cli.clearABPropsLocked()
	raw := cli.getConnState(ctx, connStateABProps)
	if raw == nil {
		return
	}
	var stored storedABProps
	if err := json.Unmarshal(raw, &stored); err != nil || len(stored.Props) == 0 {
		cli.Log.Warnf("Ignoring unreadable cached AB props: %v", err)
		return
	}
	props := make(ABProps, len(stored.Props))
	for key, value := range stored.Props {
		code, err := strconv.Atoi(key)
		if err != nil || code <= 0 {
			continue
		}
		props[code] = value
	}
	cli.abPropsCache = props
	cli.abPropsHash = stored.Hash
	cli.abPropsRefreshID = stored.RefreshID
	if stored.SyncedAt > 0 {
		cli.abPropsSyncedAt = time.UnixMilli(stored.SyncedAt)
	}
	cli.abPropsRefresh = time.Duration(stored.Refresh) * time.Second
}

// resetABProps forgets the cached flags, for a newly paired device.
func (cli *Client) resetABProps() {
	cli.abPropsLock.Lock()
	defer cli.abPropsLock.Unlock()
	cli.clearABPropsLocked()
}

func (cli *Client) clearABPropsLocked() {
	cli.abPropsCache = nil
	cli.abPropsHash = ""
	cli.abPropsRefreshID = 0
	cli.abPropsSyncedAt = time.Time{}
	cli.abPropsRefresh = 0
	cli.abPropsAnnounced = false
}

// saveABProps keeps the cached flags in the store for the next run.
func (cli *Client) saveABProps(ctx context.Context) {
	cli.abPropsLock.Lock()
	if cli.abPropsCache == nil {
		cli.abPropsLock.Unlock()
		return
	}
	stored := storedABProps{
		Props:     make(map[string]string, len(cli.abPropsCache)),
		Hash:      cli.abPropsHash,
		RefreshID: cli.abPropsRefreshID,
		Refresh:   int64(cli.abPropsRefresh / time.Second),
	}
	if !cli.abPropsSyncedAt.IsZero() {
		stored.SyncedAt = cli.abPropsSyncedAt.UnixMilli()
	}
	for code, value := range cli.abPropsCache {
		stored.Props[strconv.Itoa(code)] = value
	}
	cli.abPropsLock.Unlock()
	raw, err := json.Marshal(stored)
	if err != nil {
		cli.Log.Warnf("Failed to encode AB props for the store: %v", err)
		return
	}
	cli.putConnState(ctx, connStateABProps, raw)
}
