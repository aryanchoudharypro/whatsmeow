// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
)

const (
	// SignedPreKeyRotationInterval is how often the signed prekey is
	// replaced, matching the interval WhatsApp Web's RotateKeyJob uses.
	SignedPreKeyRotationInterval = 27 * 24 * time.Hour
	// signedPreKeyServerErrorBackoff is how long to wait before trying a
	// rotation again after the server failed with a 5xx.
	signedPreKeyServerErrorBackoff = 24 * time.Hour
	// retiredSignedPreKeyCount is how many rotated-out signed prekeys are
	// kept so messages encrypted against them still decrypt.
	retiredSignedPreKeyCount = 4
	// maxSignedPreKeyID is the 24-bit ceiling of a signed prekey ID.
	maxSignedPreKeyID = 1<<24 - 1
)

// runPostLoginTasks does what WhatsApp Web does in the background after a
// login: fetch the feature flags, check the key bundle the server holds
// against the local one, and rotate the signed prekey when it is due.
func (cli *Client) runPostLoginTasks(ctx context.Context) {
	if cli.MessengerConfig != nil || !cli.IsLoggedIn() {
		return
	}
	cli.refreshABProps(ctx)
	cli.validateKeyBundleDigest(ctx)
	if err := cli.maybeRotateSignedPreKey(ctx); err != nil {
		cli.Log.Warnf("Signed prekey rotation failed: %v", err)
	}
}

// validateKeyBundleDigest asks the server for a digest of the key bundle it
// holds for this device and compares it to the local keys, like WhatsApp
// Web's digestKey. Only a server that has no bundle at all (404) leads to
// anything: the keys are uploaded again. A mismatch is logged and left alone,
// as WhatsApp Web does.
func (cli *Client) validateKeyBundleDigest(ctx context.Context) {
	resp, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "encrypt",
		Type:      iqGet,
		To:        types.ServerJID,
		Content:   []waBinary.Node{{Tag: "digest"}},
	})
	var iqErr *IQError
	if errors.As(err, &iqErr) && iqErr.Code == 404 {
		cli.Log.Warnf("Server has no key bundle for this device, uploading prekeys again")
		cli.uploadPreKeys(ctx, true)
		return
	} else if err != nil {
		cli.Log.Debugf("Failed to get key bundle digest: %v", err)
		return
	}
	digest, ok := resp.GetOptionalChildByTag("digest")
	if !ok {
		cli.Log.Debugf("Key bundle digest response has no digest")
		return
	}
	serverHash, _ := digest.GetChildByTag("hash").Content.([]byte)
	registration, _ := digest.GetChildByTag("registration").Content.([]byte)
	if len(serverHash) == 0 || len(registration) != 4 {
		cli.Log.Debugf("Key bundle digest response is missing parts")
		return
	}
	if regID := binary.BigEndian.Uint32(registration); regID != cli.Store.RegistrationID {
		cli.Log.Warnf("Key bundle digest: server has registration ID %d, local is %d", regID, cli.Store.RegistrationID)
		return
	}

	cli.uploadPreKeysLock.Lock()
	signedPreKey := cli.Store.SignedPreKey
	cli.uploadPreKeysLock.Unlock()
	hasher := sha1.New()
	hasher.Write(cli.Store.IdentityKey.Pub[:])
	hasher.Write(signedPreKey.Pub[:])
	hasher.Write(signedPreKey.Signature[:])
	list := digest.GetChildByTag("list")
	for _, child := range list.GetChildren() {
		idBytes, _ := child.Content.([]byte)
		if len(idBytes) != 3 {
			cli.Log.Debugf("Key bundle digest lists a prekey ID of %d bytes", len(idBytes))
			return
		}
		id := binary.BigEndian.Uint32(append([]byte{0}, idBytes...))
		preKey, err := cli.Store.PreKeys.GetPreKey(ctx, id)
		if err != nil || preKey == nil {
			cli.Log.Warnf("Key bundle digest: server lists prekey %d, which isn't in the local store (%v)", id, err)
			return
		}
		hasher.Write(preKey.Pub[:])
	}
	if localHash := hasher.Sum(nil); !bytes.Equal(localHash, serverHash) {
		cli.Log.Warnf("Key bundle digest mismatch (server %x, local %x)", serverHash, localHash)
		return
	}
	cli.Log.Debugf("Key bundle on the server matches the local keys")
}

func nextSignedPreKeyID(current uint32) uint32 {
	if current >= maxSignedPreKeyID {
		return 1
	}
	return current + 1
}

func (cli *Client) setSignedPreKeyRotatedAt(ctx context.Context, ts time.Time) {
	cli.connState.Lock()
	cli.connState.signedPreKeyRotatedAt = ts
	cli.connState.Unlock()
	cli.putConnState(ctx, connStateSignedPreKeyRotate, []byte(strconv.FormatInt(ts.UnixMilli(), 10)))
}

// saveOldSignedPreKeys replaces the retired signed prekeys, in memory and in
// the store. The slice is replaced rather than changed in place, because the
// Signal store reads it while decrypting.
func (cli *Client) saveOldSignedPreKeys(ctx context.Context, oldKeys []*keys.PreKey) error {
	stored := make([]storedSignedPreKey, len(oldKeys))
	for i, key := range oldKeys {
		stored[i] = storedSignedPreKey{ID: key.KeyID, Priv: key.Priv[:], Signature: key.Signature[:]}
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if cli.Store.ConnState == nil {
		return errors.New("device has no connection state store")
	}
	err = cli.Store.ConnState.PutConnState(ctx, connStateOldSignedPreKeys, raw)
	if err != nil {
		return err
	}
	cli.Store.OldSignedPreKeys = oldKeys
	return nil
}

// maybeRotateSignedPreKey replaces the signed prekey when the rotation
// interval has passed. The key made at pairing would otherwise be used
// forever; WhatsApp Web's RotateKeyJob replaces it periodically and keeps the
// old ones, so messages already on their way against an old key still
// decrypt.
//
// Nothing here can lose a key the server may be handing out: the new key and
// the outgoing one are both stored before the upload, and the new one only
// becomes current once the server accepted it.
func (cli *Client) maybeRotateSignedPreKey(ctx context.Context) error {
	if cli.Store.ID == nil || cli.Store.ConnState == nil {
		return nil
	}
	cli.connState.Lock()
	loaded := cli.connState.loaded
	last := cli.connState.signedPreKeyRotatedAt
	cli.connState.Unlock()
	if !loaded {
		return nil
	}
	now := time.Now()
	if last.IsZero() {
		// A session from before rotation existed: count from now on.
		cli.setSignedPreKeyRotatedAt(ctx, now)
		return nil
	} else if now.Sub(last) < SignedPreKeyRotationInterval {
		return nil
	}
	return cli.rotateSignedPreKey(ctx, now, cli.uploadRotatedSignedPreKey)
}

func (cli *Client) uploadRotatedSignedPreKey(ctx context.Context, key *keys.PreKey) error {
	_, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "encrypt",
		Type:      iqSet,
		To:        types.ServerJID,
		Content: []waBinary.Node{{
			Tag:     "rotate",
			Content: []waBinary.Node{preKeyToNode(key)},
		}},
	})
	return err
}

// rotateSignedPreKey makes a new signed prekey current once upload has
// delivered it to the server.
func (cli *Client) rotateSignedPreKey(ctx context.Context, now time.Time, upload func(context.Context, *keys.PreKey) error) error {
	// Prekey uploads send the current signed prekey along, so they must not
	// run while it is being replaced.
	cli.uploadPreKeysLock.Lock()
	defer cli.uploadPreKeysLock.Unlock()

	current := cli.Store.SignedPreKey
	newID := nextSignedPreKeyID(current.KeyID)
	retired := make([]*keys.PreKey, 0, len(cli.Store.OldSignedPreKeys)+2)
	var candidate *keys.PreKey
	hasCurrent := false
	for _, old := range cli.Store.OldSignedPreKeys {
		switch old.KeyID {
		case newID:
			// Staged by an earlier attempt whose outcome is unknown. The
			// server may already have it, so the same key is sent again
			// instead of a new one under the same ID.
			candidate = old
		case current.KeyID:
			hasCurrent = true
		}
		retired = append(retired, old)
	}
	if candidate == nil {
		candidate = cli.Store.IdentityKey.CreateSignedPreKey(newID)
		retired = append(retired, candidate)
	}
	if !hasCurrent {
		retired = append(retired, current)
	}
	if err := cli.saveOldSignedPreKeys(ctx, retired); err != nil {
		return fmt.Errorf("failed to store signed prekeys before rotating: %w", err)
	}

	err := upload(ctx, candidate)
	if err != nil {
		var iqErr *IQError
		if !errors.As(err, &iqErr) {
			// The request may or may not have arrived. The staged key
			// stays, and the next connection sends it again.
			return fmt.Errorf("failed to upload new signed prekey: %w", err)
		}
		if iqErr.Code == 406 || iqErr.Code == 409 {
			// The server refused this very key, so a retry needs a new one.
			withoutCandidate := make([]*keys.PreKey, 0, len(retired))
			for _, old := range retired {
				if old != candidate {
					withoutCandidate = append(withoutCandidate, old)
				}
			}
			if saveErr := cli.saveOldSignedPreKeys(ctx, withoutCandidate); saveErr != nil {
				cli.Log.Warnf("Failed to drop rejected signed prekey: %v", saveErr)
			}
		}
		if iqErr.Code < 500 {
			// A refusal would be repeated on every connection, so it uses
			// up this interval.
			cli.setSignedPreKeyRotatedAt(ctx, now)
		} else {
			cli.setSignedPreKeyRotatedAt(ctx, now.Add(signedPreKeyServerErrorBackoff-SignedPreKeyRotationInterval))
		}
		return fmt.Errorf("server rejected new signed prekey: %w", err)
	}

	cli.Store.SignedPreKey = candidate
	if err = cli.Store.Save(ctx); err != nil {
		// The server now hands out the new key while the device row still
		// names the old one. Both are in the retired list, so messages for
		// either decrypt, and the next prekey upload tells the server which
		// one is current again.
		cli.Store.SignedPreKey = current
		return fmt.Errorf("failed to save rotated signed prekey: %w", err)
	}
	cli.setSignedPreKeyRotatedAt(ctx, now)

	kept := make([]*keys.PreKey, 0, len(retired))
	for _, old := range retired {
		if old != candidate {
			kept = append(kept, old)
		}
	}
	if len(kept) > retiredSignedPreKeyCount {
		kept = kept[len(kept)-retiredSignedPreKeyCount:]
	}
	if err = cli.saveOldSignedPreKeys(ctx, kept); err != nil {
		cli.Log.Warnf("Failed to prune retired signed prekeys: %v", err)
	}
	cli.Log.Infof("Rotated signed prekey %d -> %d", current.KeyID, candidate.KeyID)
	return nil
}
