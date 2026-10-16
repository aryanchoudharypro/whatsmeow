// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"encoding/json"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// forkCacheStore is implemented by stores that can keep the device list and
// group member caches across restarts (sqlstore).
type forkCacheStore interface {
	GetForkCacheEntries(ctx context.Context, kind string, keys []string, notBefore time.Time) (map[string][]byte, error)
	PutForkCacheEntries(ctx context.Context, kind string, entries map[string][]byte) error
	DeleteForkCacheEntries(ctx context.Context, kind string, keys []string) error
	ClearForkCacheEntries(ctx context.Context, kind string) error
}

// jidCache is an in-memory cache keyed by JID that is written through to
// the database, so after a restart the first send to a group doesn't have to
// ask the server for every member's devices and the group's member list
// again. WA Web keeps both in IndexedDB and refreshes them from device and
// group notifications; entries here also expire after maxAge.
//
// It isn't safe for concurrent use: callers hold the lock that guards it.
type jidCache[V any] struct {
	cli    *Client
	kind   string
	maxAge time.Duration
	encode func(V) ([]byte, error)
	decode func([]byte) (V, error)

	mem map[types.JID]V
	// tried holds JIDs already looked up in the database, found or not.
	tried map[types.JID]struct{}
}

func newJIDCache[V any](cli *Client, kind string, maxAge time.Duration, encode func(V) ([]byte, error), decode func([]byte) (V, error)) *jidCache[V] {
	return &jidCache[V]{
		cli: cli, kind: kind, maxAge: maxAge, encode: encode, decode: decode,
		mem:   make(map[types.JID]V),
		tried: make(map[types.JID]struct{}),
	}
}

func (c *jidCache[V]) store() forkCacheStore {
	if c.cli == nil || c.cli.Store == nil {
		return nil
	}
	db, _ := c.cli.Store.Identities.(forkCacheStore)
	return db
}

func (c *jidCache[V]) ctx() context.Context {
	if c.cli.BackgroundEventCtx != nil {
		return c.cli.BackgroundEventCtx
	}
	return context.Background()
}

// Load reads the given JIDs from the database into memory with one query,
// skipping any already in memory or already looked up.
func (c *jidCache[V]) Load(jids []types.JID) {
	db := c.store()
	if db == nil {
		return
	}
	keys := make([]string, 0, len(jids))
	for _, jid := range jids {
		if _, ok := c.mem[jid]; ok {
			continue
		}
		if _, ok := c.tried[jid]; ok {
			continue
		}
		c.tried[jid] = struct{}{}
		keys = append(keys, jid.String())
	}
	if len(keys) == 0 {
		return
	}
	rows, err := db.GetForkCacheEntries(c.ctx(), c.kind, keys, time.Now().Add(-c.maxAge))
	if err != nil {
		c.cli.Log.Warnf("Failed to load cached %s: %v", c.kind, err)
		return
	}
	for key, raw := range rows {
		jid, err := types.ParseJID(key)
		if err != nil {
			continue
		}
		val, err := c.decode(raw)
		if err != nil {
			c.cli.Log.Debugf("Dropping undecodable cached %s for %s: %v", c.kind, key, err)
			continue
		}
		c.mem[jid] = val
	}
}

// Get returns the cached value, loading it from the database if needed.
func (c *jidCache[V]) Get(jid types.JID) (V, bool) {
	if val, ok := c.mem[jid]; ok {
		return val, true
	}
	c.Load([]types.JID{jid})
	val, ok := c.mem[jid]
	return val, ok
}

// Set stores a value in memory and the database.
func (c *jidCache[V]) Set(jid types.JID, val V) {
	c.SetMany(map[types.JID]V{jid: val})
}

// SetMany stores several values, writing them to the database at once.
func (c *jidCache[V]) SetMany(vals map[types.JID]V) {
	if len(vals) == 0 {
		return
	}
	entries := make(map[string][]byte, len(vals))
	for jid, val := range vals {
		c.mem[jid] = val
		c.tried[jid] = struct{}{}
		if raw, err := c.encode(val); err == nil {
			entries[jid.String()] = raw
		}
	}
	if db := c.store(); db != nil {
		if err := db.PutForkCacheEntries(c.ctx(), c.kind, entries); err != nil {
			c.cli.Log.Warnf("Failed to save cached %s: %v", c.kind, err)
		}
	}
}

// Delete drops a value from memory and the database.
func (c *jidCache[V]) Delete(jids ...types.JID) {
	if len(jids) == 0 {
		return
	}
	keys := make([]string, len(jids))
	for i, jid := range jids {
		delete(c.mem, jid)
		c.tried[jid] = struct{}{}
		keys[i] = jid.String()
	}
	if db := c.store(); db != nil {
		if err := db.DeleteForkCacheEntries(c.ctx(), c.kind, keys); err != nil {
			c.cli.Log.Warnf("Failed to delete cached %s: %v", c.kind, err)
		}
	}
}

// Clear drops every value from memory and the database.
func (c *jidCache[V]) Clear() {
	clear(c.mem)
	clear(c.tried)
	if db := c.store(); db != nil {
		if err := db.ClearForkCacheEntries(c.ctx(), c.kind); err != nil {
			c.cli.Log.Warnf("Failed to clear cached %s: %v", c.kind, err)
		}
	}
}

type storedDeviceCache struct {
	Devices []types.JID `json:"devices"`
	DHash   string      `json:"dhash,omitempty"`
}

func encodeDeviceCache(dc deviceCache) ([]byte, error) {
	return json.Marshal(storedDeviceCache{Devices: dc.devices, DHash: dc.dhash})
}

func decodeDeviceCache(raw []byte) (deviceCache, error) {
	var stored storedDeviceCache
	err := json.Unmarshal(raw, &stored)
	return deviceCache{devices: stored.Devices, dhash: stored.DHash}, err
}

func encodeGroupMetaCache(gmc *groupMetaCache) ([]byte, error) {
	return json.Marshal(gmc)
}

func decodeGroupMetaCache(raw []byte) (*groupMetaCache, error) {
	var gmc groupMetaCache
	err := json.Unmarshal(raw, &gmc)
	return &gmc, err
}

const (
	// deviceCacheMaxAge bounds how long a saved device list is trusted.
	// Device notifications keep it current in between.
	deviceCacheMaxAge = 7 * 24 * time.Hour
	// groupCacheMaxAge bounds how long a saved group member list is
	// trusted. Group notifications and the server's participant hash on
	// every send keep it current in between.
	groupCacheMaxAge = 24 * time.Hour
)
