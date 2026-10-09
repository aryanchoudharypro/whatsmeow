// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"maps"
	"strconv"

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
func (props ABProps) Bool(code int, def bool) bool {
	value, ok := props[code]
	if !ok {
		return def
	}
	return value == "1" || value == "true"
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
// came with, so a reconnect that names the same ID sends nothing. Once a full
// set is cached, a fetch sends its hash and the server answers with only what
// changed.
func (cli *Client) refreshABProps(ctx context.Context) {
	cli.abPropsLock.Lock()
	hash := cli.abPropsHash
	seeded := cli.abPropsCache != nil
	current := cli.abPropsAreCurrent()
	cli.abPropsLock.Unlock()
	if current {
		return
	}

	attrs := waBinary.Attrs{"protocol": "1"}
	// A delta is only what changed, so it is useless without the full set
	// it applies to.
	if seeded && hash != "" {
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
		return
	}
	cli.applyABProps(resp)
	cli.dispatchEvent(&events.ABPropsRefreshed{})
}

// abPropsAreCurrent is whether the cached flags are the set the server's last
// <success> named, or it named none. The caller holds abPropsLock.
func (cli *Client) abPropsAreCurrent() bool {
	if cli.abPropsCache == nil {
		return false
	}
	return cli.abPropsServerRefreshID == 0 || cli.abPropsServerRefreshID == cli.abPropsRefreshID
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
	if ag.OptionalBool("delta_update") && cli.abPropsCache != nil {
		maps.Copy(cli.abPropsCache, props)
	} else {
		cli.abPropsCache = props
	}
	if newHash := ag.OptionalString("hash"); newHash != "" {
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
