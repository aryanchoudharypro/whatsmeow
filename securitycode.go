package whatsmeow

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.mau.fi/libsignal/session"
	"google.golang.org/protobuf/encoding/protowire"

	"go.mau.fi/whatsmeow/types"
)

// SecurityCode is what two people compare to check that their chat is
// end-to-end encrypted, as WhatsApp shows it under "Verify security code".
type SecurityCode struct {
	// Digits is the 60-digit code (WA Web shows it as 12 groups of 5).
	Digits string
	// QR is the content of the QR code the other person can scan with their
	// phone (a CombinedFingerprint protobuf, version 0).
	QR []byte
}

type identityGetter interface {
	GetIdentity(ctx context.Context, address string) ([32]byte, bool, error)
}

// securityCodeCountry is WA Web's pattern for the phone identifier: the
// country code and the last digits of the number.
var securityCodeCountry = regexp.MustCompile(`^([17]|2[07]|3[0123469]|4[013456789]|5[12345678]|6[0123456]|8[1246]|9[0123458]|\d{3})\d*?(\d{4,6})$`)

func securityCodePhoneIdentifier(pn types.JID) []byte {
	if m := securityCodeCountry.FindStringSubmatch(pn.User); m != nil {
		return []byte(m[1] + m[2])
	}
	return []byte(pn.String())
}

// securityCodeHalf is one person's 30 digits: Signal's numeric fingerprint
// with 5200 SHA-512 rounds over the sorted identity keys of all their
// devices (WAWebIdentityVerificationApi).
func securityCodeHalf(identifier, keys []byte) string {
	hash := slices.Concat([]byte{0, 0}, keys, identifier)
	for range 5200 {
		sum := sha512.Sum512(slices.Concat(hash, keys))
		hash = sum[:]
	}
	var out strings.Builder
	for off := 0; off < 30; off += 5 {
		n := uint64(binary.BigEndian.Uint32(hash[off:]))*256 + uint64(hash[off+4])
		fmt.Fprintf(&out, "%05d", n%100000)
	}
	return out.String()
}

func identityKeysBinary(keys [][32]byte) []byte {
	encoded := make([][]byte, len(keys))
	for i, key := range keys {
		encoded[i] = append([]byte{0x05}, key[:]...)
	}
	slices.SortFunc(encoded, bytes.Compare)
	return slices.Concat(encoded...)
}

func appendFingerprintData(b []byte, num protowire.Number, publicKey, pnIdentifier, lidIdentifier []byte) []byte {
	var data []byte
	data = protowire.AppendTag(data, 1, protowire.BytesType)
	data = protowire.AppendBytes(data, publicKey)
	if pnIdentifier != nil {
		data = protowire.AppendTag(data, 2, protowire.BytesType)
		data = protowire.AppendBytes(data, pnIdentifier)
	}
	if lidIdentifier != nil {
		data = protowire.AppendTag(data, 3, protowire.BytesType)
		data = protowire.AppendBytes(data, lidIdentifier)
	}
	data = protowire.AppendTag(data, 5, protowire.VarintType)
	data = protowire.AppendVarint(data, 0) // E2EE, not hosted
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, data)
}

// GetSecurityCode works out the security code between you and user the way
// WA Web does: the identity keys of every device of theirs and of yours,
// fetching keys (and starting sessions) for devices we haven't talked to.
func (cli *Client) GetSecurityCode(ctx context.Context, user types.JID) (*SecurityCode, error) {
	if cli == nil || cli.Store.ID == nil || cli.Store.IdentityKey == nil {
		return nil, ErrNotLoggedIn
	}
	getter, ok := cli.Store.Identities.(identityGetter)
	if !ok {
		return nil, errors.New("identity store can't look up keys")
	}
	user = user.ToNonAD()
	ownPN, ownLID := cli.getOwnID(), cli.getOwnLID()
	var remotePN, remoteLID types.JID
	switch user.Server {
	case types.HiddenUserServer:
		remoteLID = user
		remotePN, _ = cli.Store.LIDs.GetPNForLID(ctx, user)
	case types.DefaultUserServer:
		remotePN = user
		remoteLID, _ = cli.Store.LIDs.GetLIDForPN(ctx, user)
	default:
		return nil, fmt.Errorf("can't verify %s", user)
	}
	isOwn := func(jid types.JID) bool {
		return (!ownPN.IsEmpty() && jid.User == ownPN.User) || (!ownLID.IsEmpty() && jid.User == ownLID.User)
	}
	isMe := isOwn(user)
	remoteQuery := remotePN
	if !remoteLID.IsEmpty() {
		remoteQuery = remoteLID
	}
	ownQuery := ownPN.ToNonAD()
	if !ownLID.IsEmpty() {
		ownQuery = ownLID.ToNonAD()
	}
	devices, err := cli.GetUserDevicesContext(ctx, []types.JID{remoteQuery, ownQuery})
	if err != nil {
		return nil, fmt.Errorf("failed to get devices: %w", err)
	}

	var pnDevices []types.JID
	for _, jid := range devices {
		if jid.Server == types.DefaultUserServer {
			pnDevices = append(pnDevices, jid)
		}
	}
	lidMappings, _ := cli.Store.LIDs.GetManyLIDsForPNs(ctx, pnDevices)
	type device struct {
		jid, address types.JID
		own          bool
	}
	var list []device
	seen := map[string]bool{}
	for _, jid := range devices {
		if jid.Server == types.HostedServer || jid.Server == types.HostedLIDServer || jid.Device == 99 {
			continue
		}
		own := isOwn(jid)
		if own && (jid.Device == ownPN.Device || (!ownLID.IsEmpty() && jid.Device == ownLID.Device)) {
			continue // this device: its key is ours
		}
		address := jid
		if lid, ok := lidMappings[jid]; ok && !lid.IsEmpty() {
			cli.migrateSessionStore(ctx, jid, lid)
			address = lid
		}
		key := address.SignalAddress().String()
		if seen[key] {
			continue
		}
		seen[key] = true
		list = append(list, device{jid, address, own})
	}

	var missing []types.JID
	for _, d := range list {
		if _, found, err := getter.GetIdentity(ctx, d.address.SignalAddress().String()); err != nil {
			return nil, err
		} else if !found {
			missing = append(missing, d.jid)
		}
	}
	if len(missing) > 0 {
		bundles := cli.fetchPreKeysNoError(ctx, missing)
		for _, d := range list {
			if bundle := bundles[d.jid]; bundle != nil {
				builder := session.NewBuilderFromSignal(cli.Store, d.address.SignalAddress(), pbSerializer)
				if err := builder.ProcessBundle(ctx, bundle); err != nil {
					cli.Log.Warnf("Failed to process prekey bundle for %s while getting security code: %v", d.jid, err)
				}
			}
		}
	}

	ownKey := *cli.Store.IdentityKey.Pub
	localKeys := [][32]byte{ownKey}
	var remoteKeys [][32]byte
	if isMe {
		remoteKeys = append(remoteKeys, ownKey)
	}
	for _, d := range list {
		key, found, err := getter.GetIdentity(ctx, d.address.SignalAddress().String())
		if err != nil {
			return nil, err
		} else if !found {
			return nil, fmt.Errorf("no identity key for %s", d.jid)
		}
		if !d.own || isMe {
			remoteKeys = append(remoteKeys, key)
		}
		if d.own {
			localKeys = append(localKeys, key)
		}
	}
	if len(remoteKeys) == 0 {
		return nil, fmt.Errorf("no devices found for %s", user)
	}
	localBinary, remoteBinary := identityKeysBinary(localKeys), identityKeysBinary(remoteKeys)

	// V3 (phone numbers) when both are known, else V4 (LIDs).
	var localID, remoteID []byte
	if !ownPN.IsEmpty() && !remotePN.IsEmpty() {
		localID, remoteID = securityCodePhoneIdentifier(ownPN.ToNonAD()), securityCodePhoneIdentifier(remotePN)
	} else if !ownLID.IsEmpty() && !remoteLID.IsEmpty() {
		localID, remoteID = []byte(ownLID.User), []byte(remoteLID.User)
	} else {
		return nil, fmt.Errorf("no phone number or LID for %s", user)
	}
	halves := []string{securityCodeHalf(localID, localBinary), securityCodeHalf(remoteID, remoteBinary)}
	slices.Sort(halves)

	var localPN, remotePNID, localLIDID, remoteLIDID []byte
	if !remotePN.IsEmpty() {
		remotePNID = securityCodePhoneIdentifier(remotePN)
		if !ownPN.IsEmpty() {
			localPN = securityCodePhoneIdentifier(ownPN.ToNonAD())
		}
	}
	if !ownLID.IsEmpty() && !remoteLID.IsEmpty() {
		localLIDID = []byte(ownLID.User + "@" + types.HiddenUserServer)
		remoteLIDID = []byte(remoteLID.User + "@" + types.HiddenUserServer)
	}
	var qr []byte
	qr = protowire.AppendTag(qr, 1, protowire.VarintType)
	qr = protowire.AppendVarint(qr, 0)
	qr = appendFingerprintData(qr, 2, localBinary, localPN, localLIDID)
	qr = appendFingerprintData(qr, 3, remoteBinary, remotePNID, remoteLIDID)
	return &SecurityCode{Digits: halves[0] + halves[1], QR: qr}, nil
}
