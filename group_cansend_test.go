// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestGroupCanSendMatchesAdminByDeviceJID(t *testing.T) {
	admin := types.NewJID("111", types.HiddenUserServer)
	adminDevice := types.NewADJID("111", 0, 5)
	adminDevice.Server = types.HiddenUserServer
	member := types.NewADJID("222", 0, 3)
	member.Server = types.HiddenUserServer
	gmc := &groupMetaCache{ReadOnly: true, Admins: []types.JID{admin}}
	text := &waE2E.Message{Conversation: new(string)}

	if !gmc.CanSend(adminDevice, text) {
		t.Error("admin sending from a linked device was refused in an admins-only group")
	}
	if gmc.CanSend(member, text) {
		t.Error("non-admin was allowed to send text in an admins-only group")
	}
	if !gmc.CanSend(member, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{}}) {
		t.Error("non-admin was refused a reaction in an admins-only group")
	}
}
