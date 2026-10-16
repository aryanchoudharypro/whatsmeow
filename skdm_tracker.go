// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"

	"go.mau.fi/libsignal/protocol"

	"go.mau.fi/whatsmeow/types"
)

// Group messages used to carry our sender key, pairwise encrypted, to every
// member device on every send: in a 256-member group about 640 extra
// encryptions and some 100 KB per message. WA Web sends it only to devices
// that don't have it yet, and so does this now. A device counts as having it
// once a message carrying it was accepted by the server, for as long as our
// sender key stays the same (records are tied to its fingerprint, so
// rotating the key makes every device need it again). A device stops
// counting when it sends a retry receipt, its device list changes, or its
// user's identity changes; a device we have no session with always gets it.

// senderKeyDeviceStore is implemented by stores that keep these records
// (sqlstore).
type senderKeyDeviceStore interface {
	GetSenderKeyDevices(ctx context.Context, group, fingerprint string) ([]string, error)
	PutSenderKeyDevices(ctx context.Context, group, fingerprint string, userKeys map[string]string) error
	ForgetSenderKeyDevice(ctx context.Context, group, device string) error
	ForgetSenderKeyUser(ctx context.Context, user string) error
}

type skdmGroupState struct {
	fingerprint string
	warm        map[string]struct{}
}

type skdmTracker struct {
	lock   sync.Mutex
	groups map[types.JID]*skdmGroupState
}

func skdmDeviceKey(jid types.JID) string {
	return jid.User + "@" + jid.Server + ":" + strconv.Itoa(int(jid.Device))
}

func skdmUserKey(jid types.JID) string {
	return jid.User + "@" + jid.Server
}

// senderKeyFingerprint identifies our current sender key for a group.
func senderKeyFingerprint(skdm *protocol.SenderKeyDistributionMessage) string {
	h := sha256.New()
	var id [4]byte
	binary.BigEndian.PutUint32(id[:], skdm.ID())
	h.Write(id[:])
	if key := skdm.SignatureKey(); key != nil {
		h.Write(key.Serialize())
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (cli *Client) skdmStore() senderKeyDeviceStore {
	if cli.Store == nil {
		return nil
	}
	db, _ := cli.Store.Identities.(senderKeyDeviceStore)
	return db
}

// skdmWarmDevices returns the devices recorded as holding the sender key
// with the given fingerprint. The caller holds cli.skdm.lock.
func (cli *Client) skdmWarmDevices(ctx context.Context, group types.JID, fingerprint string) map[string]struct{} {
	if cli.skdm.groups == nil {
		cli.skdm.groups = make(map[types.JID]*skdmGroupState)
	}
	if state, ok := cli.skdm.groups[group]; ok && state.fingerprint == fingerprint {
		return state.warm
	}
	state := &skdmGroupState{fingerprint: fingerprint, warm: make(map[string]struct{})}
	if db := cli.skdmStore(); db != nil {
		devices, err := db.GetSenderKeyDevices(ctx, group.String(), fingerprint)
		if err != nil {
			// Unknown means everyone gets the key, as before.
			cli.Log.Warnf("Failed to load sender key devices of %s: %v", group, err)
		}
		for _, device := range devices {
			state.warm[device] = struct{}{}
		}
	}
	cli.skdm.groups[group] = state
	return state.warm
}

// skdmNeededFilter returns the filter for encryptMessageForDevices that
// keeps the devices still needing our sender key. A device is skipped only
// when it and its user's primary device (device 0) both hold it, as in WA
// Web, so a reinstalled phone gets it again along with its companions.
func (cli *Client) skdmNeededFilter(ctx context.Context, group types.JID, fingerprint string) func(types.JID, bool) bool {
	cli.skdm.lock.Lock()
	warm := cli.skdmWarmDevices(ctx, group, fingerprint)
	snapshot := make(map[string]struct{}, len(warm))
	for device := range warm {
		snapshot[device] = struct{}{}
	}
	cli.skdm.lock.Unlock()
	return func(device types.JID, hasSession bool) bool {
		if !hasSession {
			return true
		}
		if _, ok := snapshot[skdmDeviceKey(device)]; !ok {
			return true
		}
		primary := device
		primary.Device = 0
		_, primaryWarm := snapshot[skdmDeviceKey(primary)]
		return !primaryWarm
	}
}

// markSKDMDelivered records that devices received our sender key with the
// given fingerprint in a message the server accepted.
func (cli *Client) markSKDMDelivered(ctx context.Context, group types.JID, fingerprint string, devices []types.JID) {
	if len(devices) == 0 {
		return
	}
	userKeys := make(map[string]string, len(devices))
	for _, device := range devices {
		userKeys[skdmDeviceKey(device)] = skdmUserKey(device)
	}
	cli.skdm.lock.Lock()
	warm := cli.skdmWarmDevices(ctx, group, fingerprint)
	for device := range userKeys {
		warm[device] = struct{}{}
	}
	cli.skdm.lock.Unlock()
	if db := cli.skdmStore(); db != nil {
		if err := db.PutSenderKeyDevices(ctx, group.String(), fingerprint, userKeys); err != nil {
			cli.Log.Warnf("Failed to save sender key devices of %s: %v", group, err)
		}
	}
}

// alternateJIDs returns jid and, if known, the same device under its other
// address (LID for a phone number and vice versa).
func (cli *Client) alternateJIDs(ctx context.Context, jid types.JID) []types.JID {
	out := []types.JID{jid}
	var alt types.JID
	switch jid.Server {
	case types.DefaultUserServer:
		alt, _ = cli.Store.LIDs.GetLIDForPN(ctx, jid.ToNonAD())
	case types.HiddenUserServer:
		alt, _ = cli.Store.LIDs.GetPNForLID(ctx, jid.ToNonAD())
	}
	if !alt.IsEmpty() {
		alt.Device = jid.Device
		out = append(out, alt)
	}
	return out
}

// forgetSKDMDevice makes a device need our sender key again, in one group or
// (with an empty group) all of them. Memory and database change under the
// same lock, so a concurrent first load can't bring a forgotten record back.
func (cli *Client) forgetSKDMDevice(ctx context.Context, group, device types.JID) {
	if cli.Store == nil {
		return
	}
	devices := cli.alternateJIDs(ctx, device)
	db := cli.skdmStore()
	groupStr := ""
	if !group.IsEmpty() {
		groupStr = group.String()
	}
	cli.skdm.lock.Lock()
	defer cli.skdm.lock.Unlock()
	for groupJID, state := range cli.skdm.groups {
		if group.IsEmpty() || groupJID == group {
			for _, d := range devices {
				delete(state.warm, skdmDeviceKey(d))
			}
		}
	}
	if db == nil {
		return
	}
	for _, d := range devices {
		if err := db.ForgetSenderKeyDevice(ctx, groupStr, skdmDeviceKey(d)); err != nil {
			cli.Log.Warnf("Failed to forget sender key records of %s: %v", d, err)
		}
	}
}

// forgetSKDMUser makes every device of a user need our sender key again, in
// every group.
func (cli *Client) forgetSKDMUser(ctx context.Context, user types.JID) {
	if cli.Store == nil {
		return
	}
	users := cli.alternateJIDs(ctx, user.ToNonAD())
	db := cli.skdmStore()
	cli.skdm.lock.Lock()
	defer cli.skdm.lock.Unlock()
	for _, state := range cli.skdm.groups {
		for device := range state.warm {
			for _, u := range users {
				if strings.HasPrefix(device, skdmUserKey(u)+":") {
					delete(state.warm, device)
				}
			}
		}
	}
	if db == nil {
		return
	}
	for _, u := range users {
		if err := db.ForgetSenderKeyUser(ctx, skdmUserKey(u)); err != nil {
			cli.Log.Warnf("Failed to forget sender key records of %s: %v", u, err)
		}
	}
}
