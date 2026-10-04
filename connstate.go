// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
)

// Keys in the device's ConnStateStore.
const (
	connStateLoginCounter       = "login_counter"
	connStateEdgeRouting        = "edge_routing"
	connStateServerCertChain    = "server_cert_chain"
	connStateSignedPreKeyRotate = "signed_prekey_rotated_at"
	connStateOldSignedPreKeys   = "old_signed_prekeys"
)

// ikFailureThreshold is how many rejected IK resumes it takes before the
// client stops trying IK for the rest of the process. It is deliberately not
// persisted, like WhatsApp Web's counter, which resets on every start.
const ikFailureThreshold = 1

// connState is what the client carries from one connection to the next, the
// way WhatsApp Web does: the edge routing hint the server handed out, the
// server's certificate chain for Noise IK resumes, and when the signed prekey
// was last rotated. The login counter and the retired signed prekeys live on
// the store.Device itself, because the store package reads them.
type connState struct {
	sync.Mutex
	// loadedFor is the device the state was loaded for. A different (or
	// newly paired) device loads its own.
	loadedFor types.JID
	loaded    bool

	edgeRouting []byte
	certChain   *serverCertChain
	ikFailures  int

	signedPreKeyRotatedAt time.Time
}

// storedSignedPreKey is a retired signed prekey as it is persisted.
type storedSignedPreKey struct {
	ID        uint32 `json:"id"`
	Priv      []byte `json:"priv"`
	Signature []byte `json:"signature"`
}

func (cli *Client) getConnState(ctx context.Context, key string) []byte {
	if cli.Store.ConnState == nil {
		return nil
	}
	value, err := cli.Store.ConnState.GetConnState(ctx, key)
	if err != nil {
		cli.Log.Warnf("Failed to read %s from the connection state store: %v", key, err)
		return nil
	}
	return value
}

func (cli *Client) putConnState(ctx context.Context, key string, value []byte) {
	if cli.Store.ConnState == nil || cli.Store.ID == nil {
		return
	}
	err := cli.Store.ConnState.PutConnState(ctx, key, value)
	if err != nil {
		cli.Log.Warnf("Failed to save %s to the connection state store: %v", key, err)
	}
}

// loadConnState reads the persisted connection state for the current device.
// It does nothing before pairing, and nothing when it was already loaded for
// this device. A store that keeps nothing still works: the state then only
// lasts as long as the process.
func (cli *Client) loadConnState(ctx context.Context) {
	if cli.Store.ID == nil {
		return
	}
	cs := &cli.connState
	cs.Lock()
	defer cs.Unlock()
	if cs.loaded && cs.loadedFor == *cli.Store.ID {
		return
	}
	cs.loaded = true
	cs.loadedFor = *cli.Store.ID
	cs.edgeRouting = nil
	cs.certChain = nil
	cs.ikFailures = 0
	cs.signedPreKeyRotatedAt = time.Time{}

	// A session from before the counter was kept has been logging in with
	// 1 every time, so it carries on from there.
	cli.Store.LoginCounter = 1
	if raw := cli.getConnState(ctx, connStateLoginCounter); raw != nil {
		counter, err := strconv.ParseInt(string(raw), 10, 32)
		if err == nil && counter >= 0 {
			cli.Store.LoginCounter = int32(counter)
		}
	}
	cs.edgeRouting = cli.getConnState(ctx, connStateEdgeRouting)
	if raw := cli.getConnState(ctx, connStateServerCertChain); raw != nil {
		var chain serverCertChain
		if err := json.Unmarshal(raw, &chain); err != nil {
			cli.Log.Warnf("Ignoring unreadable cached server certificate chain: %v", err)
		} else {
			cs.certChain = &chain
		}
	}
	if raw := cli.getConnState(ctx, connStateSignedPreKeyRotate); raw != nil {
		ms, err := strconv.ParseInt(string(raw), 10, 64)
		if err == nil && ms > 0 {
			cs.signedPreKeyRotatedAt = time.UnixMilli(ms)
		}
	}
	cli.Store.OldSignedPreKeys = nil
	if raw := cli.getConnState(ctx, connStateOldSignedPreKeys); raw != nil {
		var stored []storedSignedPreKey
		if err := json.Unmarshal(raw, &stored); err != nil {
			cli.Log.Warnf("Ignoring unreadable retired signed prekeys: %v", err)
		}
		for _, item := range stored {
			if len(item.Priv) != 32 || len(item.Signature) != 64 {
				continue
			}
			cli.Store.OldSignedPreKeys = append(cli.Store.OldSignedPreKeys, &keys.PreKey{
				KeyPair:   *keys.NewKeyPairFromPrivateKey(*(*[32]byte)(item.Priv)),
				KeyID:     item.ID,
				Signature: (*[64]byte)(item.Signature),
			})
		}
	}
}

// initConnStateAfterPair starts a freshly paired device's connection state.
// Its login counter starts at zero: the first login after pairing is the
// first one the server sees.
func (cli *Client) initConnStateAfterPair(ctx context.Context) {
	cs := &cli.connState
	cs.Lock()
	cs.loaded = true
	cs.loadedFor = *cli.Store.ID
	cs.edgeRouting = nil
	cs.certChain = nil
	cs.ikFailures = 0
	cs.signedPreKeyRotatedAt = time.Now()
	cs.Unlock()
	cli.Store.LoginCounter = 0
	cli.Store.OldSignedPreKeys = nil
	cli.putConnState(ctx, connStateLoginCounter, []byte("0"))
	cli.putConnState(ctx, connStateSignedPreKeyRotate, []byte(strconv.FormatInt(time.Now().UnixMilli(), 10)))
}

// bumpLoginCounter counts a successful login. WhatsApp Web bumps the counter
// it sends as ClientPayload.lc after each authenticated connection, and only
// for a device that is already paired.
func (cli *Client) bumpLoginCounter(ctx context.Context) {
	cs := &cli.connState
	cs.Lock()
	defer cs.Unlock()
	if !cs.loaded || cli.Store.ID == nil {
		return
	}
	if cli.Store.LoginCounter < 1<<31-1 {
		cli.Store.LoginCounter++
	}
	cli.putConnState(ctx, connStateLoginCounter, []byte(strconv.FormatInt(int64(cli.Store.LoginCounter), 10)))
}

// resumeHints are the things a reconnect can reuse from earlier connections.
type resumeHints struct {
	// edgeRouting is sent ahead of the connection header so the edge server
	// can route the connection to where the account lives.
	edgeRouting []byte
	// serverStatic is the server's static key for a Noise IK resume.
	serverStatic *[32]byte
}

func (hints resumeHints) any() bool {
	return len(hints.edgeRouting) > 0 || hints.serverStatic != nil
}

// getResumeHints returns what this connection can reuse. Only a paired
// WhatsApp (not Messenger) session resumes anything.
func (cli *Client) getResumeHints(ctx context.Context) (hints resumeHints) {
	if cli.Store.ID == nil || cli.MessengerConfig != nil || cli.DisableConnectionResume {
		return
	}
	cli.loadConnState(ctx)
	cs := &cli.connState
	cs.Lock()
	defer cs.Unlock()
	if len(cs.edgeRouting) > 0 && len(cs.edgeRouting) <= socket.MaxEdgeRoutingLength {
		hints.edgeRouting = cs.edgeRouting
	}
	if cs.ikFailures < ikFailureThreshold && cs.certChain.usableAt(time.Now()) {
		hints.serverStatic = (*[32]byte)(cs.certChain.Leaf.Key)
	}
	return
}

// dropResumeHints forgets the hints a failed handshake used, so the retry
// (and later connections) start clean. A failed IK resume isn't tried again
// in this process, and unless it merely timed out, the cached chain goes too:
// a full handshake caches a fresh one.
func (cli *Client) dropResumeHints(ctx context.Context, hints resumeHints, handshakeErr error) {
	cs := &cli.connState
	cs.Lock()
	defer cs.Unlock()
	if hints.serverStatic != nil {
		cs.ikFailures++
		if !errors.Is(handshakeErr, errHandshakeTimeout) {
			cli.Log.Warnf("Noise IK resume failed, forgetting the cached server certificate chain")
			cs.certChain = nil
			cli.putConnState(ctx, connStateServerCertChain, nil)
		}
	}
	if len(hints.edgeRouting) > 0 {
		cs.edgeRouting = nil
		cli.putConnState(ctx, connStateEdgeRouting, nil)
	}
}

// handshakeSucceeded records the outcome of a completed handshake: a verified
// chain is cached for the next IK resume, and the failure count starts over.
func (cli *Client) handshakeSucceeded(ctx context.Context, chain *serverCertChain) {
	if cli.Store.ID == nil || cli.MessengerConfig != nil {
		return
	}
	cs := &cli.connState
	cs.Lock()
	defer cs.Unlock()
	cs.ikFailures = 0
	if chain == nil || !cs.loaded {
		return
	}
	cs.certChain = chain
	raw, err := json.Marshal(chain)
	if err != nil {
		cli.Log.Warnf("Failed to encode server certificate chain: %v", err)
		return
	}
	cli.putConnState(ctx, connStateServerCertChain, raw)
}

// storeEdgeRouting keeps the routing info from an <ib><edge_routing> stanza
// for the next connection.
func (cli *Client) storeEdgeRouting(ctx context.Context, routingInfo []byte) {
	if len(routingInfo) == 0 || len(routingInfo) > socket.MaxEdgeRoutingLength {
		return
	}
	cs := &cli.connState
	cs.Lock()
	defer cs.Unlock()
	if !cs.loaded {
		return
	}
	cs.edgeRouting = routingInfo
	cli.putConnState(ctx, connStateEdgeRouting, routingInfo)
}
