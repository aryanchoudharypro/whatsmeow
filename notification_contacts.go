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
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// handleContactsNotification handles <notification type="contacts">. Only
// <modify> (a contact moved to a new phone number) is acted on, as in WA Web's
// WAWebHandleContactNotification: the LID-PN pairs it carries are stored
// (learning source contact-notification) and events.ContactNumberChanged is
// dispatched.
func (cli *Client) handleContactsNotification(ctx context.Context, node *waBinary.Node) {
	if _, ok := node.GetOptionalChildByTag("sync"); ok {
		go cli.syncContactLIDs(context.WithoutCancel(ctx))
	}
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

// syncContactLIDs is the part of WA Web's doFullContactSync (the server's
// <sync> contacts notification) that matters here: every saved number whose
// LID isn't known is asked for, in chunks of 500.
func (cli *Client) syncContactLIDs(ctx context.Context) {
	if cli.Store.Contacts == nil || cli.Store.LIDs == nil {
		return
	}
	all, err := cli.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		cli.Log.Warnf("Failed to list contacts for a contact sync: %v", err)
		return
	}
	var missing []types.JID
	for jid, info := range all {
		if jid.Server != types.DefaultUserServer || (info.FullName == "" && info.FirstName == "") {
			continue
		}
		if lid, err := cli.Store.LIDs.GetLIDForPN(ctx, jid); err == nil && !lid.IsEmpty() {
			continue
		}
		missing = append(missing, jid)
	}
	const chunk = 500
	for len(missing) > 0 {
		n := min(chunk, len(missing))
		cli.recheckLIDs(ctx, missing[:n])
		missing = missing[n:]
	}
}
