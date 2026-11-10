// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"fmt"
	"strconv"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// TryFetchPrivacySettings will fetch the user's privacy settings, either from the in-memory cache or from the server.
func (cli *Client) TryFetchPrivacySettings(ctx context.Context, ignoreCache bool) (*types.PrivacySettings, error) {
	if cli == nil {
		return nil, ErrClientIsNil
	} else if val := cli.privacySettingsCache.Load(); val != nil && !ignoreCache {
		return val.(*types.PrivacySettings), nil
	}
	resp, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "privacy",
		Type:      iqGet,
		To:        types.ServerJID,
		Content:   []waBinary.Node{{Tag: "privacy"}},
	})
	if err != nil {
		return nil, err
	}
	privacyNode, ok := resp.GetOptionalChildByTag("privacy")
	if !ok {
		return nil, &ElementMissingError{Tag: "privacy", In: "response to privacy settings query"}
	}
	var settings types.PrivacySettings
	cli.parsePrivacySettings(&privacyNode, &settings)
	cli.privacySettingsCache.Store(&settings)
	return &settings, nil
}

// GetPrivacySettings will get the user's privacy settings. If an error occurs while fetching them, the error will be
// logged, but the method will just return an empty struct.
func (cli *Client) GetPrivacySettings(ctx context.Context) (settings types.PrivacySettings) {
	if cli == nil || cli.MessengerConfig != nil {
		return
	}
	settingsPtr, err := cli.TryFetchPrivacySettings(ctx, false)
	if err != nil {
		cli.Log.Errorf("Failed to fetch privacy settings: %v", err)
	} else {
		settings = *settingsPtr
	}
	return
}

// SetPrivacySetting will set the given privacy setting to the given value.
// The privacy settings will be fetched from the server after the change and the new settings will be returned.
// If an error occurs while fetching the new settings, will return an empty struct.
func (cli *Client) SetPrivacySetting(ctx context.Context, name types.PrivacySettingType, value types.PrivacySetting) (settings types.PrivacySettings, err error) {
	settingsPtr, err := cli.TryFetchPrivacySettings(ctx, false)
	if err != nil {
		return settings, err
	}
	_, err = cli.sendIQ(ctx, infoQuery{
		Namespace: "privacy",
		Type:      iqSet,
		To:        types.ServerJID,
		Content: []waBinary.Node{{
			Tag: "privacy",
			Content: []waBinary.Node{{
				Tag: "category",
				Attrs: waBinary.Attrs{
					"name":  string(name),
					"value": string(value),
				},
			}},
		}},
	})
	if err != nil {
		return settings, err
	}
	settings = *settingsPtr
	switch name {
	case types.PrivacySettingTypeGroupAdd:
		settings.GroupAdd = value
	case types.PrivacySettingTypeLastSeen:
		settings.LastSeen = value
	case types.PrivacySettingTypeStatus:
		settings.Status = value
	case types.PrivacySettingTypeProfile:
		settings.Profile = value
	case types.PrivacySettingTypeReadReceipts:
		settings.ReadReceipts = value
	case types.PrivacySettingTypeOnline:
		settings.Online = value
	case types.PrivacySettingTypeCallAdd:
		settings.CallAdd = value
	}
	cli.privacySettingsCache.Store(&settings)
	return
}

// SetDefaultDisappearingTimer will set the default disappearing message timer.
func (cli *Client) SetDefaultDisappearingTimer(ctx context.Context, timer time.Duration) (err error) {
	_, err = cli.sendIQ(ctx, infoQuery{
		Namespace: "disappearing_mode",
		Type:      iqSet,
		To:        types.ServerJID,
		Content: []waBinary.Node{{
			Tag: "disappearing_mode",
			Attrs: waBinary.Attrs{
				"duration": strconv.Itoa(int(timer.Seconds())),
			},
		}},
	})
	return
}

func (cli *Client) parsePrivacySettings(privacyNode *waBinary.Node, settings *types.PrivacySettings) *events.PrivacySettings {
	var evt events.PrivacySettings
	for _, child := range privacyNode.GetChildren() {
		if child.Tag != "category" {
			continue
		}
		ag := child.AttrGetter()
		name := types.PrivacySettingType(ag.String("name"))
		value := types.PrivacySetting(ag.String("value"))
		switch name {
		case types.PrivacySettingTypeGroupAdd:
			settings.GroupAdd = value
			evt.GroupAddChanged = true
		case types.PrivacySettingTypeLastSeen:
			settings.LastSeen = value
			evt.LastSeenChanged = true
		case types.PrivacySettingTypeStatus:
			settings.Status = value
			evt.StatusChanged = true
		case types.PrivacySettingTypeProfile:
			settings.Profile = value
			evt.ProfileChanged = true
		case types.PrivacySettingTypeReadReceipts:
			settings.ReadReceipts = value
			evt.ReadReceiptsChanged = true
		case types.PrivacySettingTypeOnline:
			settings.Online = value
			evt.OnlineChanged = true
		case types.PrivacySettingTypeCallAdd:
			settings.CallAdd = value
			evt.CallAddChanged = true
		case types.PrivacySettingTypeMessages:
			settings.Messages = value
			evt.MessagesChanged = true
		case types.PrivacySettingTypeDefense:
			settings.Defense = value
			evt.DefenseChanged = true
		case types.PrivacySettingTypeStickers:
			settings.Stickers = value
			evt.StickersChanged = true
		}
	}
	return &evt
}

func (cli *Client) handlePrivacySettingsNotification(ctx context.Context, privacyNode *waBinary.Node) {
	cli.Log.Debugf("Parsing privacy settings change notification")
	settings, err := cli.TryFetchPrivacySettings(ctx, false)
	if err != nil {
		cli.Log.Errorf("Failed to fetch privacy settings when handling change: %v", err)
		return
	}
	evt := cli.parsePrivacySettings(privacyNode, settings)
	cli.privacySettingsCache.Store(settings)
	cli.dispatchEvent(evt)
}

// GetPrivacyDisallowedList fetches who is left out of a "My contacts except..."
// privacy setting (value contact_blacklist). Only last, profile, status and
// groupadd have such a list.
//
// The returned DHash identifies this version of the list; pass it to
// SetPrivacyDisallowedList so the server can refuse a change made against an
// outdated list.
func (cli *Client) GetPrivacyDisallowedList(ctx context.Context, name types.PrivacySettingType) (*types.PrivacyDisallowedList, error) {
	if cli == nil {
		return nil, ErrClientIsNil
	}
	// Like WA Web's queryPrivacyDisallowedList: ask with LID addressing first
	// and fall back to the old phone number request if that fails.
	list, err := cli.queryPrivacyDisallowedList(ctx, name, true)
	if err != nil {
		cli.Log.Debugf("LID query for %s privacy list failed, trying phone numbers: %v", name, err)
		var pnErr error
		list, pnErr = cli.queryPrivacyDisallowedList(ctx, name, false)
		if pnErr != nil {
			return nil, fmt.Errorf("%w (phone number query: %v)", err, pnErr)
		}
	}
	return list, nil
}

func (cli *Client) queryPrivacyDisallowedList(ctx context.Context, name types.PrivacySettingType, lidMode bool) (*types.PrivacyDisallowedList, error) {
	privacy := waBinary.Node{
		Tag: "privacy",
		Content: []waBinary.Node{{
			Tag:   "list",
			Attrs: waBinary.Attrs{"name": string(name), "value": string(types.PrivacySettingContactBlacklist)},
		}},
	}
	if lidMode {
		privacy.Attrs = waBinary.Attrs{"addressing_mode": string(types.AddressingModeLID)}
	}
	resp, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "privacy",
		Type:      iqGet,
		To:        types.ServerJID,
		Content:   []waBinary.Node{privacy},
	})
	if err != nil {
		return nil, err
	}
	privacyNode, ok := resp.GetOptionalChildByTag("privacy")
	if !ok {
		return nil, &ElementMissingError{Tag: "privacy", In: "response to privacy list query"}
	}
	return parsePrivacyDisallowedList(&privacyNode, name), nil
}

// parsePrivacyDisallowedList reads both reply shapes: with
// addressing_mode="lid" each user's jid is an optional LID with the phone
// number in pn_jid, otherwise jid is the phone number with the LID in lid.
func parsePrivacyDisallowedList(privacyNode *waBinary.Node, name types.PrivacySettingType) *types.PrivacyDisallowedList {
	list := &types.PrivacyDisallowedList{Name: name}
	for _, listNode := range privacyNode.GetChildrenByTag("list") {
		ag := listNode.AttrGetter()
		if listName := ag.OptionalString("name"); listName != "" && listName != string(name) {
			continue
		}
		list.DHash = ag.OptionalString("dhash")
		for _, userNode := range listNode.GetChildrenByTag("user") {
			uag := userNode.AttrGetter()
			var user types.PrivacyListUser
			jid, _ := uag.GetJID("jid", false)
			pn, _ := uag.GetJID("pn_jid", false)
			lid, _ := uag.GetJID("lid", false)
			if jid.Server == types.DefaultUserServer && pn.IsEmpty() {
				pn, jid = jid, lid
			}
			user.JID, user.PN = jid, pn
			if user.JID.IsEmpty() {
				user.JID = pn
			}
			if !user.JID.IsEmpty() {
				list.Users = append(list.Users, user)
			}
		}
	}
	return list
}

// SetPrivacyDisallowedList sets a privacy setting to "My contacts except..."
// (contact_blacklist), adding and removing the given people from its list,
// the way WhatsApp Web's WAWebSetPrivacyJob does. Each user should be given
// by LID, with their phone number JID in PN when known.
//
// dhash is the list's hash from GetPrivacyDisallowedList (empty if there is
// no list yet). With no changes, the setting is switched to the list as the
// server already has it. The new hash is returned.
func (cli *Client) SetPrivacyDisallowedList(ctx context.Context, name types.PrivacySettingType, dhash string, changes []types.PrivacyListChange) (string, error) {
	if cli == nil {
		return "", ErrClientIsNil
	}
	category := waBinary.Node{
		Tag: "category",
		Attrs: waBinary.Attrs{
			"name":  string(name),
			"value": string(types.PrivacySettingContactBlacklist),
		},
	}
	if dhash == "" {
		dhash = "none"
	}
	category.Attrs["dhash"] = dhash
	users := make([]waBinary.Node, 0, len(changes))
	for _, change := range changes {
		attrs := waBinary.Attrs{"action": string(change.Action), "jid": change.JID.ToNonAD()}
		if !change.PN.IsEmpty() {
			attrs["pn_jid"] = change.PN.ToNonAD()
		}
		users = append(users, waBinary.Node{Tag: "user", Attrs: attrs})
	}
	if len(users) > 0 {
		category.Content = users
	}
	resp, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "privacy",
		Type:      iqSet,
		To:        types.ServerJID,
		Content: []waBinary.Node{{
			Tag:     "privacy",
			Attrs:   waBinary.Attrs{"addressing_mode": string(types.AddressingModeLID)},
			Content: []waBinary.Node{category},
		}},
	})
	if err != nil {
		return "", err
	}
	newHash := ""
	if privacyNode, ok := resp.GetOptionalChildByTag("privacy"); ok {
		for _, child := range privacyNode.GetChildrenByTag("category") {
			cag := child.AttrGetter()
			if cag.OptionalString("value") == "error" {
				if errNode, ok := child.GetOptionalChildByTag("error"); ok {
					return "", fmt.Errorf("server refused the list change: %s %s", errNode.AttrGetter().OptionalString("code"), errNode.AttrGetter().OptionalString("text"))
				}
				return "", fmt.Errorf("server refused the list change")
			}
			newHash = cag.OptionalString("dhash")
		}
	}
	if val := cli.privacySettingsCache.Load(); val != nil {
		settings := *val.(*types.PrivacySettings)
		switch name {
		case types.PrivacySettingTypeGroupAdd:
			settings.GroupAdd = types.PrivacySettingContactBlacklist
		case types.PrivacySettingTypeLastSeen:
			settings.LastSeen = types.PrivacySettingContactBlacklist
		case types.PrivacySettingTypeStatus:
			settings.Status = types.PrivacySettingContactBlacklist
		case types.PrivacySettingTypeProfile:
			settings.Profile = types.PrivacySettingContactBlacklist
		}
		cli.privacySettingsCache.Store(&settings)
	}
	return newHash, nil
}

// GetDefaultDisappearingTimer gets the default disappearing message timer
// new chats start with (zero when it's off), as a usync query for our own
// disappearing_mode.
func (cli *Client) GetDefaultDisappearingTimer(ctx context.Context) (time.Duration, error) {
	ownID := cli.getOwnID().ToNonAD()
	if ownID.IsEmpty() {
		return 0, ErrNotLoggedIn
	}
	list, err := cli.usync(ctx, []types.JID{ownID}, "query", "interactive", []waBinary.Node{{Tag: "disappearing_mode"}})
	if err != nil {
		return 0, err
	}
	for _, child := range list.GetChildren() {
		if child.Tag != "user" {
			continue
		}
		mode, ok := child.GetOptionalChildByTag("disappearing_mode")
		if !ok {
			continue
		}
		ag := mode.AttrGetter()
		return time.Duration(ag.OptionalInt("duration")) * time.Second, nil
	}
	return 0, nil
}

// SetReadReceiptsPrivacy updates the cached read receipts privacy setting
// without asking the server. WA Web does this with the readreceipts attribute
// the server puts on the ack of a read receipt, so a change made on another
// device takes effect before the next privacy fetch.
func (cli *Client) SetReadReceiptsPrivacy(value types.PrivacySetting) {
	if cli == nil || value == "" {
		return
	}
	if val := cli.privacySettingsCache.Load(); val != nil {
		settings := *val.(*types.PrivacySettings)
		if settings.ReadReceipts == value {
			return
		}
		settings.ReadReceipts = value
		cli.privacySettingsCache.Store(&settings)
	}
}
