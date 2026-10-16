// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"maps"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// memDeviceCache and memGroupCache build caches with no database behind
// them, for tests.
func memDeviceCache(vals map[types.JID]deviceCache) *jidCache[deviceCache] {
	c := newJIDCache(nil, "device_list", deviceCacheMaxAge, encodeDeviceCache, decodeDeviceCache)
	maps.Copy(c.mem, vals)
	return c
}

func memGroupCache(vals map[types.JID]*groupMetaCache) *jidCache[*groupMetaCache] {
	c := newJIDCache(nil, "group_meta", groupCacheMaxAge, encodeGroupMetaCache, decodeGroupMetaCache)
	maps.Copy(c.mem, vals)
	return c
}

func TestCachedValuesRoundTrip(t *testing.T) {
	dc := deviceCache{devices: []types.JID{types.NewADJID("15550000001", 0, 0), types.NewADJID("100000000000001", 1, 4)}, dhash: "2:abc"}
	raw, err := encodeDeviceCache(dc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeDeviceCache(raw)
	if err != nil || len(got.devices) != 2 || got.devices[1] != dc.devices[1] || got.dhash != dc.dhash {
		t.Fatalf("device cache round trip: %+v %v", got, err)
	}
	gmc := &groupMetaCache{
		AddressingMode: types.AddressingModeLID, ReadOnly: true, CommunityAnnouncementGroup: true,
		Members: []types.JID{types.NewJID("100000000000001", types.HiddenUserServer)},
		Admins:  []types.JID{types.NewJID("100000000000001", types.HiddenUserServer)},
	}
	raw, err = encodeGroupMetaCache(gmc)
	if err != nil {
		t.Fatal(err)
	}
	gotGroup, err := decodeGroupMetaCache(raw)
	if err != nil || gotGroup.AddressingMode != gmc.AddressingMode || !gotGroup.ReadOnly || !gotGroup.CommunityAnnouncementGroup ||
		len(gotGroup.Members) != 1 || gotGroup.Members[0] != gmc.Members[0] || gotGroup.Admins[0] != gmc.Admins[0] {
		t.Fatalf("group cache round trip: %+v %v", gotGroup, err)
	}
}
