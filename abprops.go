// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"strconv"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// Some of the server's feature flags ("AB props"), by config code, as
// WhatsApp Web names them.
const (
	ABPropChannelsPinAdminEnabled    = 29516 // channels_message_pin_admin_enabled
	ABPropChannelsPinFollowerEnabled = 29517 // channels_message_pin_follower_enabled
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
