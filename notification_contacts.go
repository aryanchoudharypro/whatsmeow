// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
)

// handleContactsNotification handles <notification type="contacts">. Only
// <modify> (a contact moved to a new phone number) is acted on, as in WA Web's
// WAWebHandleContactNotification: the LID-PN pairs it carries are stored
// (learning source contact-notification) and events.ContactNumberChanged is
// dispatched.
func (cli *Client) handleContactsNotification(ctx context.Context, node *waBinary.Node) {
	modify, ok := node.GetOptionalChildByTag("modify")
	if !ok {
		return
	}
	ag := modify.AttrGetter()
	oldPN := ag.OptionalJIDOrEmpty("old").ToNonAD()
	newPN := ag.OptionalJIDOrEmpty("new").ToNonAD()
	oldLID := ag.OptionalJIDOrEmpty("old_lid").ToNonAD()
	newLID := ag.OptionalJIDOrEmpty("new_lid").ToNonAD()
	if oldPN.IsEmpty() || newPN.IsEmpty() {
		cli.Log.Debugf("Contacts modify notification without old/new number: %s", node)
		return
	}
	if !oldLID.IsEmpty() && !newLID.IsEmpty() {
		mappings := []store.LIDMapping{{LID: newLID, PN: newPN}}
		if oldLID != newLID {
			mappings = append([]store.LIDMapping{{LID: oldLID, PN: oldPN}}, mappings...)
		}
		if err := cli.Store.LIDs.PutManyLIDMappings(ctx, mappings); err != nil {
			cli.Log.Errorf("Failed to store LID mappings from number change %s -> %s: %v", oldPN, newPN, err)
		}
	}
	cli.dispatchEvent(&events.ContactNumberChanged{
		OldJID:    oldPN,
		NewJID:    newPN,
		OldLID:    oldLID,
		NewLID:    newLID,
		Timestamp: node.AttrGetter().UnixTime("t"),
	})
}
