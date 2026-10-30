// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package sqlstore contains an SQL-backed implementation of the interfaces in the store package.
package sqlstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/exslices"
	"go.mau.fi/util/exsync"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
)

// ErrInvalidLength is returned by some database getters if the database returned a byte array with an unexpected length.
// This should be impossible, as the database schema contains CHECK()s for all the relevant columns.
var ErrInvalidLength = errors.New("database returned byte array with illegal length")

// PostgresArrayWrapper is a function to wrap array values before passing them to the sql package.
//
// When using github.com/lib/pq, you should set
//
//	whatsmeow.PostgresArrayWrapper = pq.Array
var PostgresArrayWrapper func(any) interface {
	driver.Valuer
	sql.Scanner
}

type SQLStore struct {
	*Container
	JID string

	preKeyLock sync.Mutex

	contactCache     map[types.JID]*types.ContactInfo
	contactCacheLock sync.Mutex

	migratedPNSessionsCache *exsync.Set[string]

	// identityCache holds identity keys known to match the database, so the
	// per-message trust check and the save after every encrypt/decrypt don't
	// each hit SQL for a key that almost never changes.
	identityCache     map[string][32]byte
	identityCacheLock sync.RWMutex
	// identityGen counts forgets, so a database read that started before
	// one isn't cached afterwards.
	identityGen uint64
	// identityPending holds addresses written inside a transaction that may
	// not have committed yet; they aren't cached until this passes.
	identityPending map[string]time.Time
}

// NewSQLStore creates a new SQLStore with the given database container and user JID.
// It contains implementations of all the different stores in the store package.
//
// In general, you should use Container.NewDevice or Container.GetDevice instead of this.
func NewSQLStore(c *Container, jid types.JID) *SQLStore {
	return &SQLStore{
		Container:    c,
		JID:          jid.String(),
		contactCache: make(map[types.JID]*types.ContactInfo),

		migratedPNSessionsCache: exsync.NewSet[string](),
		identityCache:           make(map[string][32]byte),
		identityPending:         make(map[string]time.Time),
	}
}

var _ store.AllSessionSpecificStores = (*SQLStore)(nil)

const (
	putIdentityQuery = `
		INSERT INTO whatsmeow_identity_keys (our_jid, their_id, identity) VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, their_id) DO UPDATE SET identity=excluded.identity
	`
	deleteAllIdentitiesQuery = `DELETE FROM whatsmeow_identity_keys WHERE our_jid=$1 AND their_id LIKE $2`
	deleteIdentityQuery      = `DELETE FROM whatsmeow_identity_keys WHERE our_jid=$1 AND their_id=$2`
	getIdentityQuery         = `SELECT identity FROM whatsmeow_identity_keys WHERE our_jid=$1 AND their_id=$2`
)

// inTxn reports whether ctx carries a database transaction. Writes made
// inside one may still be rolled back, so they must not be cached.
func (s *SQLStore) inTxn(ctx context.Context) bool {
	_, ok := s.db.Execable(ctx).(dbutil.Transaction)
	return ok
}

// identityTxnWindow is how long an identity written inside a transaction
// stays uncached; the cache can't see the commit.
const identityTxnWindow = 30 * time.Second

func (s *SQLStore) cachedIdentity(address string) ([32]byte, bool) {
	s.identityCacheLock.RLock()
	key, ok := s.identityCache[address]
	s.identityCacheLock.RUnlock()
	return key, ok
}

func (s *SQLStore) identityGeneration() uint64 {
	s.identityCacheLock.RLock()
	defer s.identityCacheLock.RUnlock()
	return s.identityGen
}

// cacheIdentity caches a key read or written at generation gen, unless
// something was forgotten since or the address has an uncommitted write.
func (s *SQLStore) cacheIdentity(address string, key [32]byte, gen uint64) {
	s.identityCacheLock.Lock()
	defer s.identityCacheLock.Unlock()
	if s.identityGen != gen {
		return
	}
	if until, ok := s.identityPending[address]; ok {
		if time.Now().Before(until) {
			return
		}
		delete(s.identityPending, address)
	}
	s.identityCache[address] = key
}

func (s *SQLStore) forgetIdentity(address string) {
	s.identityCacheLock.Lock()
	delete(s.identityCache, address)
	s.identityGen++
	s.identityCacheLock.Unlock()
}

// forgetIdentityInTxn forgets an address written inside a transaction and
// keeps it out of the cache until the transaction has surely ended.
func (s *SQLStore) forgetIdentityInTxn(address string) {
	s.identityCacheLock.Lock()
	delete(s.identityCache, address)
	s.identityGen++
	s.identityPending[address] = time.Now().Add(identityTxnWindow)
	s.identityCacheLock.Unlock()
}

// forgetIdentitiesOf drops every cached device identity of a user
// (addresses of the form "user:device" or "user.agent:device").
func (s *SQLStore) forgetIdentitiesOf(user string) {
	prefix := user + ":"
	s.identityCacheLock.Lock()
	for addr := range s.identityCache {
		if strings.HasPrefix(addr, prefix) {
			delete(s.identityCache, addr)
		}
	}
	s.identityGen++
	s.identityCacheLock.Unlock()
}

func (s *SQLStore) PutIdentity(ctx context.Context, address string, key [32]byte) error {
	if cached, ok := s.cachedIdentity(address); ok && cached == key {
		return nil
	}
	inTxn := s.inTxn(ctx)
	if inTxn {
		s.forgetIdentityInTxn(address)
	}
	gen := s.identityGeneration()
	_, err := s.db.Exec(ctx, putIdentityQuery, s.JID, address, key[:])
	if err != nil {
		s.forgetIdentity(address)
		return err
	}
	if !inTxn {
		s.cacheIdentity(address, key, gen)
	}
	return nil
}

func (s *SQLStore) DeleteAllIdentities(ctx context.Context, phone string) error {
	_, err := s.db.Exec(ctx, deleteAllIdentitiesQuery, s.JID, phone+":%")
	s.forgetIdentitiesOf(phone)
	return err
}

func (s *SQLStore) DeleteIdentity(ctx context.Context, address string) error {
	_, err := s.db.Exec(ctx, deleteAllIdentitiesQuery, s.JID, address)
	s.forgetIdentity(address)
	return err
}

func (s *SQLStore) IsTrustedIdentity(ctx context.Context, address string, key [32]byte) (bool, error) {
	if cached, ok := s.cachedIdentity(address); ok {
		return cached == key, nil
	}
	gen := s.identityGeneration()
	var existingIdentity []byte
	err := s.db.QueryRow(ctx, getIdentityQuery, s.JID, address).Scan(&existingIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		// Trust if not known, it'll be saved automatically later
		return true, nil
	} else if err != nil {
		return false, err
	} else if len(existingIdentity) != 32 {
		return false, ErrInvalidLength
	}
	existing := *(*[32]byte)(existingIdentity)
	if !s.inTxn(ctx) {
		s.cacheIdentity(address, existing, gen)
	}
	return existing == key, nil
}

const getManyIdentitiesQueryGeneric = `SELECT their_id, identity FROM whatsmeow_identity_keys WHERE our_jid=$1 AND their_id IN (%s)`

// PrefetchIdentities loads the stored identity keys of the given addresses
// into the cache with one query, ahead of a group send that checks and saves
// each of them.
func (s *SQLStore) PrefetchIdentities(ctx context.Context, addresses []string) error {
	missing := make([]string, 0, len(addresses))
	s.identityCacheLock.RLock()
	for _, addr := range addresses {
		if _, ok := s.identityCache[addr]; !ok {
			missing = append(missing, addr)
		}
	}
	s.identityCacheLock.RUnlock()
	if len(missing) == 0 || s.inTxn(ctx) {
		return nil
	}
	gen := s.identityGeneration()
	// SQLite allows 32766 parameters; stay far below it.
	const chunk = 500
	for start := 0; start < len(missing); start += chunk {
		part := missing[start:min(start+chunk, len(missing))]
		args := make([]any, len(part)+1)
		placeholders := make([]string, len(part))
		args[0] = s.JID
		for i, addr := range part {
			args[i+1] = addr
			placeholders[i] = fmt.Sprintf("$%d", i+2)
		}
		rows, err := s.db.Query(ctx, fmt.Sprintf(getManyIdentitiesQueryGeneric, strings.Join(placeholders, ",")), args...)
		found := make(map[string][32]byte, len(part))
		err = sessionScanner.NewRowIter(rows, err).Iter(func(tuple addressSessionTuple) (bool, error) {
			if len(tuple.Session) == 32 {
				found[tuple.Address] = *(*[32]byte)(tuple.Session)
			}
			return true, nil
		})
		if err != nil {
			return err
		}
		for addr, key := range found {
			s.cacheIdentity(addr, key, gen)
		}
	}
	return nil
}

const (
	getSessionQuery             = `SELECT session FROM whatsmeow_sessions WHERE our_jid=$1 AND their_id=$2`
	hasSessionQuery             = `SELECT true FROM whatsmeow_sessions WHERE our_jid=$1 AND their_id=$2`
	getManySessionQueryPostgres = `SELECT their_id, session FROM whatsmeow_sessions WHERE our_jid=$1 AND their_id = ANY($2)`
	getManySessionQueryGeneric  = `SELECT their_id, session FROM whatsmeow_sessions WHERE our_jid=$1 AND their_id IN (%s)`
	putSessionQuery             = `
		INSERT INTO whatsmeow_sessions (our_jid, their_id, session) VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, their_id) DO UPDATE SET session=excluded.session
	`
	deleteAllSessionsQuery = `DELETE FROM whatsmeow_sessions WHERE our_jid=$1 AND their_id LIKE $2`
	deleteSessionQuery     = `DELETE FROM whatsmeow_sessions WHERE our_jid=$1 AND their_id=$2`

	migratePNToLIDSessionsQuery = `
		INSERT INTO whatsmeow_sessions (our_jid, their_id, session)
		SELECT our_jid, replace(their_id, $2, $3), session
		FROM whatsmeow_sessions
		WHERE our_jid=$1 AND their_id LIKE $2 || ':%'
		ON CONFLICT (our_jid, their_id) DO UPDATE SET session=excluded.session
	`
	deleteAllIdentityKeysQuery      = `DELETE FROM whatsmeow_identity_keys WHERE our_jid=$1 AND their_id LIKE $2`
	migratePNToLIDIdentityKeysQuery = `
		INSERT INTO whatsmeow_identity_keys (our_jid, their_id, identity)
		SELECT our_jid, replace(their_id, $2, $3), identity
		FROM whatsmeow_identity_keys
		WHERE our_jid=$1 AND their_id LIKE $2 || ':%'
		ON CONFLICT (our_jid, their_id) DO UPDATE SET identity=excluded.identity
	`
	deleteAllSenderKeysQuery      = `DELETE FROM whatsmeow_sender_keys WHERE our_jid=$1 AND sender_id LIKE $2`
	migratePNToLIDSenderKeysQuery = `
		INSERT INTO whatsmeow_sender_keys (our_jid, chat_id, sender_id, sender_key)
		SELECT our_jid, chat_id, replace(sender_id, $2, $3), sender_key
		FROM whatsmeow_sender_keys
		WHERE our_jid=$1 AND sender_id LIKE $2 || ':%'
		ON CONFLICT (our_jid, chat_id, sender_id) DO UPDATE SET sender_key=excluded.sender_key
	`
)

func (s *SQLStore) GetSession(ctx context.Context, address string) (session []byte, err error) {
	err = s.db.QueryRow(ctx, getSessionQuery, s.JID, address).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (s *SQLStore) HasSession(ctx context.Context, address string) (has bool, err error) {
	err = s.db.QueryRow(ctx, hasSessionQuery, s.JID, address).Scan(&has)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

type addressSessionTuple struct {
	Address string
	Session []byte
}

var sessionScanner = dbutil.ConvertRowFn[addressSessionTuple](func(row dbutil.Scannable) (out addressSessionTuple, err error) {
	err = row.Scan(&out.Address, &out.Session)
	return
})

func (s *SQLStore) GetManySessions(ctx context.Context, addresses []string) (map[string][]byte, error) {
	if len(addresses) == 0 {
		return nil, nil
	}

	var rows dbutil.Rows
	var err error
	if s.db.Dialect == dbutil.Postgres && PostgresArrayWrapper != nil {
		rows, err = s.db.Query(ctx, getManySessionQueryPostgres, s.JID, PostgresArrayWrapper(addresses))
	} else {
		args := make([]any, len(addresses)+1)
		placeholders := make([]string, len(addresses))
		args[0] = s.JID
		for i, addr := range addresses {
			args[i+1] = addr
			placeholders[i] = fmt.Sprintf("$%d", i+2)
		}
		rows, err = s.db.Query(ctx, fmt.Sprintf(getManySessionQueryGeneric, strings.Join(placeholders, ",")), args...)
	}
	result := make(map[string][]byte, len(addresses))
	for _, addr := range addresses {
		result[addr] = nil
	}
	err = sessionScanner.NewRowIter(rows, err).Iter(func(tuple addressSessionTuple) (bool, error) {
		result[tuple.Address] = tuple.Session
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *SQLStore) PutManySessions(ctx context.Context, sessions map[string][]byte) error {
	return s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		for addr, sess := range sessions {
			err := s.PutSession(ctx, addr, sess)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLStore) PutSession(ctx context.Context, address string, session []byte) error {
	_, err := s.db.Exec(ctx, putSessionQuery, s.JID, address, session)
	return err
}

func (s *SQLStore) DeleteAllSessions(ctx context.Context, phone string) error {
	return s.deleteAllSessions(ctx, phone)
}

func (s *SQLStore) deleteAllSessions(ctx context.Context, phone string) error {
	_, err := s.db.Exec(ctx, deleteAllSessionsQuery, s.JID, phone+":%")
	return err
}

func (s *SQLStore) deleteAllSenderKeys(ctx context.Context, phone string) error {
	_, err := s.db.Exec(ctx, deleteAllSenderKeysQuery, s.JID, phone+":%")
	return err
}

func (s *SQLStore) deleteAllIdentityKeys(ctx context.Context, phone string) error {
	_, err := s.db.Exec(ctx, deleteAllIdentityKeysQuery, s.JID, phone+":%")
	s.forgetIdentitiesOf(phone)
	return err
}

func (s *SQLStore) DeleteSession(ctx context.Context, address string) error {
	_, err := s.db.Exec(ctx, deleteSessionQuery, s.JID, address)
	return err
}

func (s *SQLStore) MigratePNToLID(ctx context.Context, pn, lid types.JID) error {
	pnSignal := pn.SignalAddressUser()
	if !s.migratedPNSessionsCache.Add(pnSignal) {
		return nil
	}
	var sessionsUpdated, identityKeysUpdated, senderKeysUpdated int64
	lidSignal := lid.SignalAddressUser()
	// The migration overwrites the LID's identity rows and deletes the
	// PN's; forget both once it has committed, in case a read raced it.
	defer s.forgetIdentitiesOf(lidSignal)
	defer s.forgetIdentitiesOf(pnSignal)
	err := s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		res, err := s.db.Exec(ctx, migratePNToLIDSessionsQuery, s.JID, pnSignal, lidSignal)
		if err != nil {
			return fmt.Errorf("failed to migrate sessions: %w", err)
		}
		sessionsUpdated, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected for sessions: %w", err)
		}
		err = s.deleteAllSessions(ctx, pnSignal)
		if err != nil {
			return fmt.Errorf("failed to delete extra sessions: %w", err)
		}

		res, err = s.db.Exec(ctx, migratePNToLIDIdentityKeysQuery, s.JID, pnSignal, lidSignal)
		if err != nil {
			return fmt.Errorf("failed to migrate identity keys: %w", err)
		}
		identityKeysUpdated, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected for identity keys: %w", err)
		}
		err = s.deleteAllIdentityKeys(ctx, pnSignal)
		if err != nil {
			return fmt.Errorf("failed to delete extra identity keys: %w", err)
		}

		res, err = s.db.Exec(ctx, migratePNToLIDSenderKeysQuery, s.JID, pnSignal, lidSignal)
		if err != nil {
			return fmt.Errorf("failed to migrate sender keys: %w", err)
		}
		senderKeysUpdated, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected for sender keys: %w", err)
		}
		err = s.deleteAllSenderKeys(ctx, pnSignal)
		if err != nil {
			return fmt.Errorf("failed to delete extra sender keys: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if sessionsUpdated > 0 || senderKeysUpdated > 0 || identityKeysUpdated > 0 {
		s.log.Infof("Migrated %d sessions, %d identity keys and %d sender keys from %s to %s", sessionsUpdated, identityKeysUpdated, senderKeysUpdated, pnSignal, lidSignal)
	} else {
		s.log.Debugf("No sessions or sender keys found to migrate from %s to %s", pnSignal, lidSignal)
	}
	return nil
}

const (
	getLastPreKeyIDQuery        = `SELECT MAX(key_id) FROM whatsmeow_pre_keys WHERE jid=$1`
	insertPreKeyQuery           = `INSERT INTO whatsmeow_pre_keys (jid, key_id, key, uploaded) VALUES ($1, $2, $3, $4)`
	getUnuploadedPreKeysQuery   = `SELECT key_id, key FROM whatsmeow_pre_keys WHERE jid=$1 AND uploaded=false ORDER BY key_id LIMIT $2`
	getPreKeyQuery              = `SELECT key_id, key FROM whatsmeow_pre_keys WHERE jid=$1 AND key_id=$2`
	deletePreKeyQuery           = `DELETE FROM whatsmeow_pre_keys WHERE jid=$1 AND key_id=$2`
	markPreKeysAsUploadedQuery  = `UPDATE whatsmeow_pre_keys SET uploaded=true WHERE jid=$1 AND key_id<=$2`
	getUploadedPreKeyCountQuery = `SELECT COUNT(*) FROM whatsmeow_pre_keys WHERE jid=$1 AND uploaded=true`
)

func (s *SQLStore) genOnePreKey(ctx context.Context, id uint32, markUploaded bool) (*keys.PreKey, error) {
	key := keys.NewPreKey(id)
	_, err := s.db.Exec(ctx, insertPreKeyQuery, s.JID, key.KeyID, key.Priv[:], markUploaded)
	return key, err
}

func (s *SQLStore) getNextPreKeyID(ctx context.Context) (uint32, error) {
	var lastKeyID sql.NullInt32
	err := s.db.QueryRow(ctx, getLastPreKeyIDQuery, s.JID).Scan(&lastKeyID)
	if err != nil {
		return 0, fmt.Errorf("failed to query next prekey ID: %w", err)
	}
	return uint32(lastKeyID.Int32) + 1, nil
}

func (s *SQLStore) GenOnePreKey(ctx context.Context) (*keys.PreKey, error) {
	s.preKeyLock.Lock()
	defer s.preKeyLock.Unlock()
	nextKeyID, err := s.getNextPreKeyID(ctx)
	if err != nil {
		return nil, err
	}
	return s.genOnePreKey(ctx, nextKeyID, true)
}

func (s *SQLStore) GetOrGenPreKeys(ctx context.Context, count uint32) ([]*keys.PreKey, error) {
	s.preKeyLock.Lock()
	defer s.preKeyLock.Unlock()

	newKeys, err := scanPreKey.NewRowIter(s.db.Query(ctx, getUnuploadedPreKeysQuery, s.JID, count)).AsList()
	if err != nil {
		return nil, fmt.Errorf("failed to query existing prekeys: %w", err)
	}

	alreadyGeneratedCount := uint32(len(newKeys))
	if count > alreadyGeneratedCount {
		var nextKeyID uint32
		nextKeyID, err = s.getNextPreKeyID(ctx)
		if err != nil {
			return nil, err
		}
		newKeys = slices.Grow(newKeys, int(count)-len(newKeys))[:count]
		for i := alreadyGeneratedCount; i < count; i++ {
			newKeys[i], err = s.genOnePreKey(ctx, nextKeyID, false)
			if err != nil {
				return nil, fmt.Errorf("failed to generate prekey: %w", err)
			}
			nextKeyID++
		}
	}

	return newKeys, nil
}

var scanPreKey = dbutil.ConvertRowFn[*keys.PreKey](func(row dbutil.Scannable) (*keys.PreKey, error) {
	var priv []byte
	var id uint32
	err := row.Scan(&id, &priv)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else if len(priv) != 32 {
		return nil, ErrInvalidLength
	}
	return &keys.PreKey{
		KeyPair: *keys.NewKeyPairFromPrivateKey(*(*[32]byte)(priv)),
		KeyID:   id,
	}, nil
})

func (s *SQLStore) GetPreKey(ctx context.Context, id uint32) (*keys.PreKey, error) {
	return scanPreKey(s.db.QueryRow(ctx, getPreKeyQuery, s.JID, id))
}

func (s *SQLStore) RemovePreKey(ctx context.Context, id uint32) error {
	_, err := s.db.Exec(ctx, deletePreKeyQuery, s.JID, id)
	return err
}

func (s *SQLStore) MarkPreKeysAsUploaded(ctx context.Context, upToID uint32) error {
	_, err := s.db.Exec(ctx, markPreKeysAsUploadedQuery, s.JID, upToID)
	return err
}

func (s *SQLStore) UploadedPreKeyCount(ctx context.Context) (count int, err error) {
	err = s.db.QueryRow(ctx, getUploadedPreKeyCountQuery, s.JID).Scan(&count)
	return
}

const (
	getSenderKeyQuery = `SELECT sender_key FROM whatsmeow_sender_keys WHERE our_jid=$1 AND chat_id=$2 AND sender_id=$3`
	putSenderKeyQuery = `
		INSERT INTO whatsmeow_sender_keys (our_jid, chat_id, sender_id, sender_key) VALUES ($1, $2, $3, $4)
		ON CONFLICT (our_jid, chat_id, sender_id) DO UPDATE SET sender_key=excluded.sender_key
	`
)

func (s *SQLStore) PutSenderKey(ctx context.Context, group, user string, session []byte) error {
	_, err := s.db.Exec(ctx, putSenderKeyQuery, s.JID, group, user, session)
	return err
}

func (s *SQLStore) GetSenderKey(ctx context.Context, group, user string) (key []byte, err error) {
	err = s.db.QueryRow(ctx, getSenderKeyQuery, s.JID, group, user).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

const (
	putAppStateSyncKeyQuery = `
		INSERT INTO whatsmeow_app_state_sync_keys (jid, key_id, key_data, timestamp, fingerprint) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (jid, key_id) DO UPDATE
			SET key_data=excluded.key_data, timestamp=excluded.timestamp, fingerprint=excluded.fingerprint
			WHERE excluded.timestamp > whatsmeow_app_state_sync_keys.timestamp
	`
	getAllAppStateSyncKeysQuery     = `SELECT key_data, timestamp, fingerprint FROM whatsmeow_app_state_sync_keys WHERE jid=$1 ORDER BY timestamp DESC`
	getAppStateSyncKeyQuery         = `SELECT key_data, timestamp, fingerprint FROM whatsmeow_app_state_sync_keys WHERE jid=$1 AND key_id=$2`
	getLatestAppStateSyncKeyIDQuery = `SELECT key_id FROM whatsmeow_app_state_sync_keys WHERE jid=$1 ORDER BY timestamp DESC LIMIT 1`
)

func (s *SQLStore) PutAppStateSyncKey(ctx context.Context, id []byte, key store.AppStateSyncKey) error {
	_, err := s.db.Exec(ctx, putAppStateSyncKeyQuery, s.JID, id, key.Data, key.Timestamp, key.Fingerprint)
	return err
}

var convertAppStateSyncKeyRow = dbutil.ConvertRowFn[*store.AppStateSyncKey](func(rows dbutil.Scannable) (*store.AppStateSyncKey, error) {
	var item store.AppStateSyncKey
	err := rows.Scan(&item.Data, &item.Timestamp, &item.Fingerprint)
	if err != nil {
		return nil, err
	}
	return &item, nil
})

func (s *SQLStore) GetAllAppStateSyncKeys(ctx context.Context) ([]*store.AppStateSyncKey, error) {
	return convertAppStateSyncKeyRow.NewRowIter(s.db.Query(ctx, getAllAppStateSyncKeysQuery, s.JID)).AsList()
}

func (s *SQLStore) GetAppStateSyncKey(ctx context.Context, id []byte) (*store.AppStateSyncKey, error) {
	key, err := convertAppStateSyncKeyRow(s.db.QueryRow(ctx, getAppStateSyncKeyQuery, s.JID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return key, err
}

func (s *SQLStore) GetLatestAppStateSyncKeyID(ctx context.Context) ([]byte, error) {
	var keyID []byte
	err := s.db.QueryRow(ctx, getLatestAppStateSyncKeyIDQuery, s.JID).Scan(&keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return keyID, err
}

const (
	putAppStateVersionQuery = `
		INSERT INTO whatsmeow_app_state_version (jid, name, version, hash) VALUES ($1, $2, $3, $4)
		ON CONFLICT (jid, name) DO UPDATE SET version=excluded.version, hash=excluded.hash
	`
	getAppStateVersionQuery                 = `SELECT version, hash FROM whatsmeow_app_state_version WHERE jid=$1 AND name=$2`
	deleteAppStateVersionQuery              = `DELETE FROM whatsmeow_app_state_version WHERE jid=$1 AND name=$2`
	putAppStateMutationMACsQuery            = `INSERT INTO whatsmeow_app_state_mutation_macs (jid, name, version, index_mac, value_mac) VALUES `
	deleteAppStateMutationMACsQueryPostgres = `DELETE FROM whatsmeow_app_state_mutation_macs WHERE jid=$1 AND name=$2 AND index_mac=ANY($3::bytea[])`
	deleteAppStateMutationMACsQueryGeneric  = `DELETE FROM whatsmeow_app_state_mutation_macs WHERE jid=$1 AND name=$2 AND index_mac IN `
	getAppStateMutationMACQuery             = `SELECT value_mac FROM whatsmeow_app_state_mutation_macs WHERE jid=$1 AND name=$2 AND index_mac=$3 ORDER BY version DESC LIMIT 1`
	deleteAllAppStateMutationMACsQuery      = `DELETE FROM whatsmeow_app_state_mutation_macs WHERE jid=$1 AND name=$2`
)

func (s *SQLStore) PutAppStateVersion(ctx context.Context, name string, version uint64, hash [128]byte) error {
	_, err := s.db.Exec(ctx, putAppStateVersionQuery, s.JID, name, version, hash[:])
	return err
}

func (s *SQLStore) GetAppStateVersion(ctx context.Context, name string) (version uint64, hash [128]byte, err error) {
	var uncheckedHash []byte
	err = s.db.QueryRow(ctx, getAppStateVersionQuery, s.JID, name).Scan(&version, &uncheckedHash)
	if errors.Is(err, sql.ErrNoRows) {
		// version will be 0 and hash will be an empty array, which is the correct initial state
		err = nil
	} else if err != nil {
		// There's an error, just return it
	} else if len(uncheckedHash) != 128 {
		// This shouldn't happen
		err = ErrInvalidLength
	} else if version == 0 {
		err = fmt.Errorf("invalid saved app state version 0 for name %s (hash %x)", name, uncheckedHash)
	} else {
		// No errors, convert hash slice to array
		hash = *(*[128]byte)(uncheckedHash)
	}
	return
}

func (s *SQLStore) DeleteAppStateVersion(ctx context.Context, name string) error {
	_, err := s.db.Exec(ctx, deleteAppStateVersionQuery, s.JID, name)
	return err
}

func (s *SQLStore) putAppStateMutationMACs(ctx context.Context, name string, version uint64, mutations []store.AppStateMutationMAC) error {
	values := make([]any, 3+len(mutations)*2)
	queryParts := make([]string, len(mutations))
	values[0] = s.JID
	values[1] = name
	values[2] = version
	placeholderSyntax := "($1, $2, $3, $%d, $%d)"
	if s.db.Dialect == dbutil.SQLite {
		placeholderSyntax = "(?1, ?2, ?3, ?%d, ?%d)"
	}
	for i, mutation := range mutations {
		baseIndex := 3 + i*2
		values[baseIndex] = mutation.IndexMAC
		values[baseIndex+1] = mutation.ValueMAC
		queryParts[i] = fmt.Sprintf(placeholderSyntax, baseIndex+1, baseIndex+2)
	}
	_, err := s.db.Exec(ctx, putAppStateMutationMACsQuery+strings.Join(queryParts, ","), values...)
	return err
}

const mutationBatchSize = 400

func (s *SQLStore) PutAppStateMutationMACs(ctx context.Context, name string, version uint64, mutations []store.AppStateMutationMAC) error {
	if len(mutations) == 0 {
		return nil
	}
	return s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		for slice := range slices.Chunk(mutations, mutationBatchSize) {
			err := s.putAppStateMutationMACs(ctx, name, version, slice)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLStore) DeleteAppStateMutationMACs(ctx context.Context, name string, indexMACs [][]byte) (err error) {
	if len(indexMACs) == 0 {
		return
	}
	if s.db.Dialect == dbutil.Postgres && PostgresArrayWrapper != nil {
		_, err = s.db.Exec(ctx, deleteAppStateMutationMACsQueryPostgres, s.JID, name, PostgresArrayWrapper(indexMACs))
	} else {
		args := make([]any, 2+len(indexMACs))
		args[0] = s.JID
		args[1] = name
		queryParts := make([]string, len(indexMACs))
		for i, item := range indexMACs {
			args[2+i] = item
			queryParts[i] = fmt.Sprintf("$%d", i+3)
		}
		_, err = s.db.Exec(ctx, deleteAppStateMutationMACsQueryGeneric+"("+strings.Join(queryParts, ",")+")", args...)
	}
	return
}

func (s *SQLStore) GetAppStateMutationMAC(ctx context.Context, name string, indexMAC []byte) (valueMAC []byte, err error) {
	err = s.db.QueryRow(ctx, getAppStateMutationMACQuery, s.JID, name, indexMAC).Scan(&valueMAC)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (s *SQLStore) DeleteAllAppStateMutationMACs(ctx context.Context, name string) error {
	_, err := s.db.Exec(ctx, deleteAllAppStateMutationMACsQuery, s.JID, name)
	return err
}

const (
	putContactNameQuery = `
		INSERT INTO whatsmeow_contacts (our_jid, their_jid, first_name, full_name) VALUES ($1, $2, $3, $4)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET first_name=excluded.first_name, full_name=excluded.full_name
	`
	putRedactedPhoneQuery = `
		INSERT INTO whatsmeow_contacts (our_jid, their_jid, redacted_phone)
		VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET redacted_phone=excluded.redacted_phone
	`
	putPushNameQuery = `
		INSERT INTO whatsmeow_contacts (our_jid, their_jid, push_name) VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET push_name=excluded.push_name
	`
	putBusinessNameQuery = `
		INSERT INTO whatsmeow_contacts (our_jid, their_jid, business_name) VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET business_name=excluded.business_name
	`
	getContactQuery = `
		SELECT first_name, full_name, push_name, business_name, redacted_phone FROM whatsmeow_contacts WHERE our_jid=$1 AND their_jid=$2
	`
	getAllContactsQuery = `
		SELECT their_jid, first_name, full_name, push_name, business_name, redacted_phone FROM whatsmeow_contacts WHERE our_jid=$1
	`
)

var putContactNamesMassInsertBuilder = dbutil.NewMassInsertBuilder[store.ContactEntry, [1]any](
	putContactNameQuery, "($1, $%d, $%d, $%d)",
)

var putRedactedPhonesMassInsertBuilder = dbutil.NewMassInsertBuilder[store.RedactedPhoneEntry, [1]any](
	putRedactedPhoneQuery, "($1, $%d, $%d)",
)

func (s *SQLStore) PutPushName(ctx context.Context, user types.JID, pushName string) (bool, string, error) {
	s.contactCacheLock.Lock()
	defer s.contactCacheLock.Unlock()

	cached, err := s.getContact(ctx, user)
	if err != nil {
		return false, "", err
	}
	if cached.PushName != pushName {
		_, err = s.db.Exec(ctx, putPushNameQuery, s.JID, user, pushName)
		if err != nil {
			return false, "", err
		}
		previousName := cached.PushName
		cached.PushName = pushName
		cached.Found = true
		return true, previousName, nil
	}
	return false, "", nil
}

func (s *SQLStore) PutBusinessName(ctx context.Context, user types.JID, businessName string) (bool, string, error) {
	s.contactCacheLock.Lock()
	defer s.contactCacheLock.Unlock()

	cached, err := s.getContact(ctx, user)
	if err != nil {
		return false, "", err
	}
	if cached.BusinessName != businessName {
		_, err = s.db.Exec(ctx, putBusinessNameQuery, s.JID, user, businessName)
		if err != nil {
			return false, "", err
		}
		previousName := cached.BusinessName
		cached.BusinessName = businessName
		cached.Found = true
		return true, previousName, nil
	}
	return false, "", nil
}

func (s *SQLStore) PutContactName(ctx context.Context, user types.JID, firstName, fullName string) error {
	s.contactCacheLock.Lock()
	defer s.contactCacheLock.Unlock()

	cached, err := s.getContact(ctx, user)
	if err != nil {
		return err
	}
	if cached.FirstName != firstName || cached.FullName != fullName {
		_, err = s.db.Exec(ctx, putContactNameQuery, s.JID, user, firstName, fullName)
		if err != nil {
			return err
		}
		cached.FirstName = firstName
		cached.FullName = fullName
		cached.Found = true
	}
	return nil
}

const contactBatchSize = 300

func (s *SQLStore) PutAllContactNames(ctx context.Context, contacts []store.ContactEntry) error {
	if len(contacts) == 0 {
		return nil
	}
	origLen := len(contacts)
	// Deduplication keeps the first entry, but the last one is the newest
	slices.Reverse(contacts)
	contacts = exslices.DeduplicateUnsortedOverwriteFunc(contacts, func(t store.ContactEntry) types.JID {
		return t.JID
	})
	if origLen != len(contacts) {
		s.log.Warnf("%d duplicate contacts found in PutAllContactNames", origLen-len(contacts))
	}
	err := s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		for slice := range slices.Chunk(contacts, contactBatchSize) {
			query, vars := putContactNamesMassInsertBuilder.Build([1]any{s.JID}, slice)
			_, err := s.db.Exec(ctx, query, vars...)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.contactCacheLock.Lock()
	// Just clear the cache, fetching pushnames and business names would be too much effort
	s.contactCache = make(map[types.JID]*types.ContactInfo)
	s.contactCacheLock.Unlock()
	return nil
}

func (s *SQLStore) PutManyRedactedPhones(ctx context.Context, entries []store.RedactedPhoneEntry) error {
	if len(entries) == 0 {
		return nil
	}
	origLen := len(entries)
	entries = exslices.DeduplicateUnsortedOverwriteFunc(entries, func(t store.RedactedPhoneEntry) types.JID {
		return t.JID
	})
	if origLen != len(entries) {
		s.log.Warnf("%d duplicate contacts found in PutManyRedactedPhones", origLen-len(entries))
	}
	err := s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		for slice := range slices.Chunk(entries, contactBatchSize) {
			query, vars := putRedactedPhonesMassInsertBuilder.Build([1]any{s.JID}, slice)
			_, err := s.db.Exec(ctx, query, vars...)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.contactCacheLock.Lock()
	for _, entry := range entries {
		if cached, ok := s.contactCache[entry.JID]; ok && cached.RedactedPhone == entry.RedactedPhone {
			continue
		}
		delete(s.contactCache, entry.JID)
	}
	s.contactCacheLock.Unlock()
	return nil
}

func (s *SQLStore) getContact(ctx context.Context, user types.JID) (*types.ContactInfo, error) {
	cached, ok := s.contactCache[user]
	if ok {
		return cached, nil
	}

	var first, full, push, business, redactedPhone sql.NullString
	err := s.db.QueryRow(ctx, getContactQuery, s.JID, user).Scan(&first, &full, &push, &business, &redactedPhone)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	info := &types.ContactInfo{
		Found:         err == nil,
		FirstName:     first.String,
		FullName:      full.String,
		PushName:      push.String,
		BusinessName:  business.String,
		RedactedPhone: redactedPhone.String,
	}
	s.contactCache[user] = info
	return info, nil
}

func (s *SQLStore) GetContact(ctx context.Context, user types.JID) (types.ContactInfo, error) {
	s.contactCacheLock.Lock()
	info, err := s.getContact(ctx, user)
	s.contactCacheLock.Unlock()
	if err != nil {
		return types.ContactInfo{}, err
	}
	return *info, nil
}

type contactTuple struct {
	JID  types.JID
	Info *types.ContactInfo
}

var convertContactRow = dbutil.ConvertRowFn[*contactTuple](func(rows dbutil.Scannable) (*contactTuple, error) {
	var jid types.JID
	var first, full, push, business, redactedPhone sql.NullString
	err := rows.Scan(&jid, &first, &full, &push, &business, &redactedPhone)
	if err != nil {
		return nil, fmt.Errorf("error scanning row: %w", err)
	}
	return &contactTuple{
		JID: jid,
		Info: &types.ContactInfo{
			Found:         true,
			FirstName:     first.String,
			FullName:      full.String,
			PushName:      push.String,
			BusinessName:  business.String,
			RedactedPhone: redactedPhone.String,
		},
	}, nil
})

func (s *SQLStore) GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	// Not copied into contactCache: that would keep every contact in memory
	// for the rest of the session, when lookups only ever need a few of them
	// (getContact caches those as they're asked for).
	output := make(map[types.JID]types.ContactInfo)
	err := convertContactRow.NewRowIter(s.db.Query(ctx, getAllContactsQuery, s.JID)).Iter(func(tuple *contactTuple) (bool, error) {
		output[tuple.JID] = *tuple.Info
		return true, nil
	})
	return output, err
}

const (
	putChatSettingQuery = `
		INSERT INTO whatsmeow_chat_settings (our_jid, chat_jid, %[1]s) VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, chat_jid) DO UPDATE SET %[1]s=excluded.%[1]s
	`
	getChatSettingsQuery = `
		SELECT muted_until, pinned, archived FROM whatsmeow_chat_settings WHERE our_jid=$1 AND chat_jid=$2
	`
)

func (s *SQLStore) PutMutedUntil(ctx context.Context, chat types.JID, mutedUntil time.Time) error {
	var val int64
	if mutedUntil == store.MutedForever {
		val = -1
	} else if !mutedUntil.IsZero() {
		val = mutedUntil.Unix()
	}
	_, err := s.db.Exec(ctx, fmt.Sprintf(putChatSettingQuery, "muted_until"), s.JID, chat, val)
	return err
}

func (s *SQLStore) PutPinned(ctx context.Context, chat types.JID, pinned bool) error {
	_, err := s.db.Exec(ctx, fmt.Sprintf(putChatSettingQuery, "pinned"), s.JID, chat, pinned)
	return err
}

func (s *SQLStore) PutArchived(ctx context.Context, chat types.JID, archived bool) error {
	_, err := s.db.Exec(ctx, fmt.Sprintf(putChatSettingQuery, "archived"), s.JID, chat, archived)
	return err
}

func (s *SQLStore) PutWASARootSecretID(ctx context.Context, chat types.JID, id types.MessageID) error {
	_, err := s.db.Exec(ctx, fmt.Sprintf(putChatSettingQuery, "wasa_root_secret_id"), s.JID, chat, id)
	return err
}

func (s *SQLStore) GetWASARootSecretID(ctx context.Context, chat types.JID) (id types.MessageID, err error) {
	err = s.db.QueryRow(ctx, `SELECT wasa_root_secret_id FROM whatsmeow_chat_settings WHERE our_jid=$1 AND chat_jid=$2`, s.JID, chat).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (s *SQLStore) GetChatSettings(ctx context.Context, chat types.JID) (settings types.LocalChatSettings, err error) {
	var mutedUntil int64
	err = s.db.QueryRow(ctx, getChatSettingsQuery, s.JID, chat).Scan(&mutedUntil, &settings.Pinned, &settings.Archived)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	} else if err != nil {
		return
	} else {
		settings.Found = true
	}
	if mutedUntil < 0 {
		settings.MutedUntil = store.MutedForever
	} else if mutedUntil > 0 {
		settings.MutedUntil = time.Unix(mutedUntil, 0)
	}
	return
}

const (
	putMsgSecret = `
		INSERT INTO whatsmeow_message_secrets (our_jid, chat_jid, sender_jid, message_id, key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (our_jid, chat_jid, sender_jid, message_id) DO NOTHING
	`
	getMsgSecret = `
		SELECT key, sender_jid
		FROM whatsmeow_message_secrets
		WHERE our_jid=$1 AND (chat_jid=$2 OR chat_jid=(
			CASE
				WHEN $2 LIKE '%@lid'
					THEN (SELECT pn || '@s.whatsapp.net' FROM whatsmeow_lid_map WHERE lid=replace($2, '@lid', ''))
				WHEN $2 LIKE '%@s.whatsapp.net'
					THEN (SELECT lid || '@lid' FROM whatsmeow_lid_map WHERE pn=replace($2, '@s.whatsapp.net', ''))
			END
		)) AND message_id=$4 AND (sender_jid=$3 OR sender_jid=(
			CASE
				WHEN $3 LIKE '%@lid'
					THEN (SELECT pn || '@s.whatsapp.net' FROM whatsmeow_lid_map WHERE lid=replace($3, '@lid', ''))
				WHEN $3 LIKE '%@s.whatsapp.net'
					THEN (SELECT lid || '@lid' FROM whatsmeow_lid_map WHERE pn=replace($3, '@s.whatsapp.net', ''))
			END
		))
	`
)

func (s *SQLStore) PutMessageSecrets(ctx context.Context, inserts []store.MessageSecretInsert) (err error) {
	if len(inserts) == 0 {
		return nil
	}
	return s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		for _, insert := range inserts {
			if insert.Chat.IsEmpty() || insert.Sender.IsEmpty() || insert.ID == "" || len(insert.Secret) == 0 {
				continue
			}
			_, err = s.db.Exec(ctx, putMsgSecret, s.JID, insert.Chat.ToNonAD(), insert.Sender.ToNonAD(), insert.ID, insert.Secret)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLStore) PutMessageSecret(ctx context.Context, chat, sender types.JID, id types.MessageID, secret []byte) (err error) {
	_, err = s.db.Exec(ctx, putMsgSecret, s.JID, chat.ToNonAD(), sender.ToNonAD(), id, secret)
	return
}

func (s *SQLStore) GetMessageSecret(ctx context.Context, chat, sender types.JID, id types.MessageID) (secret []byte, realSender types.JID, err error) {
	err = s.db.QueryRow(ctx, getMsgSecret, s.JID, chat.ToNonAD(), sender.ToNonAD(), id).Scan(&secret, &realSender)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

const (
	putPrivacyTokens = `
		INSERT INTO whatsmeow_privacy_tokens (our_jid, their_jid, token, timestamp, sender_timestamp)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET
			token=EXCLUDED.token,
			timestamp=EXCLUDED.timestamp,
			sender_timestamp=COALESCE(EXCLUDED.sender_timestamp, whatsmeow_privacy_tokens.sender_timestamp)
		WHERE EXCLUDED.timestamp >= whatsmeow_privacy_tokens.timestamp
	`
	getPrivacyToken = `
		SELECT token, timestamp, sender_timestamp FROM whatsmeow_privacy_tokens WHERE our_jid=$1 AND (their_jid=$2 OR their_jid=(
			CASE
				WHEN $2 LIKE '%@lid'
					THEN (SELECT pn || '@s.whatsapp.net' FROM whatsmeow_lid_map WHERE lid=replace($2, '@lid', ''))
				WHEN $2 LIKE '%@s.whatsapp.net'
					THEN (SELECT lid || '@lid' FROM whatsmeow_lid_map WHERE pn=replace($2, '@s.whatsapp.net', ''))
				ELSE $2
			END
		))
		ORDER BY timestamp DESC LIMIT 1
	`
	deleteExpiredPrivacyTokens = `
		DELETE FROM whatsmeow_privacy_tokens
		WHERE our_jid=$1 AND timestamp < $2
	`
)

const (
	putNCTSaltQuery = `
		INSERT INTO whatsmeow_nct_salt (our_jid, salt) VALUES ($1, $2)
		ON CONFLICT (our_jid) DO UPDATE SET salt=excluded.salt
	`
	getNCTSaltQuery    = `SELECT salt FROM whatsmeow_nct_salt WHERE our_jid=$1`
	deleteNCTSaltQuery = `DELETE FROM whatsmeow_nct_salt WHERE our_jid=$1`
)

func (s *SQLStore) PutPrivacyTokens(ctx context.Context, tokens ...store.PrivacyToken) error {
	args := make([]any, 1+len(tokens)*4)
	placeholders := make([]string, len(tokens))
	args[0] = s.JID
	for i, token := range tokens {
		args[i*4+1] = token.User.ToNonAD().String()
		args[i*4+2] = token.Token
		args[i*4+3] = token.Timestamp.Unix()
		if token.SenderTimestamp.IsZero() {
			args[i*4+4] = nil
		} else {
			args[i*4+4] = token.SenderTimestamp.Unix()
		}
		placeholders[i] = fmt.Sprintf("($1, $%d, $%d, $%d, $%d)", i*4+2, i*4+3, i*4+4, i*4+5)
	}
	query := strings.ReplaceAll(putPrivacyTokens, "($1, $2, $3, $4, $5)", strings.Join(placeholders, ","))
	_, err := s.db.Exec(ctx, query, args...)
	return err
}

func (s *SQLStore) GetPrivacyToken(ctx context.Context, user types.JID) (*store.PrivacyToken, error) {
	var token store.PrivacyToken
	token.User = user.ToNonAD()
	var ts int64
	var senderTS sql.NullInt64
	err := s.db.QueryRow(ctx, getPrivacyToken, s.JID, token.User).Scan(&token.Token, &ts, &senderTS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else {
		token.Timestamp = time.Unix(ts, 0)
		if senderTS.Valid {
			token.SenderTimestamp = time.Unix(senderTS.Int64, 0)
		}
		return &token, nil
	}
}

func (s *SQLStore) PutNCTSalt(ctx context.Context, salt []byte) error {
	_, err := s.db.Exec(ctx, putNCTSaltQuery, s.JID, salt)
	return err
}

func (s *SQLStore) GetNCTSalt(ctx context.Context) ([]byte, error) {
	var salt []byte
	err := s.db.QueryRow(ctx, getNCTSaltQuery, s.JID).Scan(&salt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return salt, nil
}

func (s *SQLStore) DeleteNCTSalt(ctx context.Context) error {
	_, err := s.db.Exec(ctx, deleteNCTSaltQuery, s.JID)
	return err
}

const (
	putConnStateQuery = `
		INSERT INTO whatsmeow_fork_conn_state (our_jid, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, key) DO UPDATE SET value=excluded.value
	`
	getConnStateQuery    = `SELECT value FROM whatsmeow_fork_conn_state WHERE our_jid=$1 AND key=$2`
	deleteConnStateQuery = `DELETE FROM whatsmeow_fork_conn_state WHERE our_jid=$1 AND key=$2`
)

func (s *SQLStore) GetConnState(ctx context.Context, key string) ([]byte, error) {
	var value []byte
	err := s.db.QueryRow(ctx, getConnStateQuery, s.JID, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return value, nil
}

func (s *SQLStore) PutConnState(ctx context.Context, key string, value []byte) error {
	var err error
	if value == nil {
		_, err = s.db.Exec(ctx, deleteConnStateQuery, s.JID, key)
	} else {
		_, err = s.db.Exec(ctx, putConnStateQuery, s.JID, key, value)
	}
	return err
}

const (
	getTCTokenIssuedQuery = `SELECT issued_at FROM whatsmeow_fork_tctoken_issued WHERE our_jid=$1 AND their_jid=$2`
	putTCTokenIssuedQuery = `
		INSERT INTO whatsmeow_fork_tctoken_issued (our_jid, their_jid, issued_at) VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET issued_at=excluded.issued_at
		WHERE excluded.issued_at > whatsmeow_fork_tctoken_issued.issued_at
	`
	deleteExpiredTCTokenIssuedQuery = `DELETE FROM whatsmeow_fork_tctoken_issued WHERE our_jid=$1 AND issued_at < $2`
)

const (
	getForkCacheQuery = `SELECT key, value FROM whatsmeow_fork_cache WHERE our_jid=$1 AND kind=$2 AND updated_at >= $3 AND key IN (%s)`
	putForkCacheQuery = `
		INSERT INTO whatsmeow_fork_cache (our_jid, kind, key, value, updated_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (our_jid, kind, key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
	`
	deleteForkCacheQuery = `DELETE FROM whatsmeow_fork_cache WHERE our_jid=$1 AND kind=$2 AND key IN (%s)`
	clearForkCacheQuery  = `DELETE FROM whatsmeow_fork_cache WHERE our_jid=$1 AND kind=$2`
)

// forkCacheChunk keeps IN (...) lists well under SQLite's parameter limit.
const forkCacheChunk = 500

func inPlaceholders(start, n int) string {
	placeholders := make([]string, n)
	for i := range placeholders {
		placeholders[i] = fmt.Sprintf("$%d", start+i)
	}
	return strings.Join(placeholders, ",")
}

// GetForkCacheEntries returns the cached values of the given keys stored at
// or after notBefore.
func (s *SQLStore) GetForkCacheEntries(ctx context.Context, kind string, keys []string, notBefore time.Time) (map[string][]byte, error) {
	result := make(map[string][]byte)
	for start := 0; start < len(keys); start += forkCacheChunk {
		part := keys[start:min(start+forkCacheChunk, len(keys))]
		args := make([]any, 0, len(part)+3)
		args = append(args, s.JID, kind, notBefore.Unix())
		for _, key := range part {
			args = append(args, key)
		}
		rows, err := s.db.Query(ctx, fmt.Sprintf(getForkCacheQuery, inPlaceholders(4, len(part))), args...)
		err = sessionScanner.NewRowIter(rows, err).Iter(func(tuple addressSessionTuple) (bool, error) {
			result[tuple.Address] = tuple.Session
			return true, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// PutForkCacheEntries stores cached values.
func (s *SQLStore) PutForkCacheEntries(ctx context.Context, kind string, entries map[string][]byte) error {
	now := time.Now().Unix()
	return s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		for key, value := range entries {
			if _, err := s.db.Exec(ctx, putForkCacheQuery, s.JID, kind, key, value, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteForkCacheEntries removes cached values.
func (s *SQLStore) DeleteForkCacheEntries(ctx context.Context, kind string, keys []string) error {
	for start := 0; start < len(keys); start += forkCacheChunk {
		part := keys[start:min(start+forkCacheChunk, len(keys))]
		args := make([]any, 0, len(part)+2)
		args = append(args, s.JID, kind)
		for _, key := range part {
			args = append(args, key)
		}
		if _, err := s.db.Exec(ctx, fmt.Sprintf(deleteForkCacheQuery, inPlaceholders(3, len(part))), args...); err != nil {
			return err
		}
	}
	return nil
}

// ClearForkCacheEntries removes every cached value of a kind.
func (s *SQLStore) ClearForkCacheEntries(ctx context.Context, kind string) error {
	_, err := s.db.Exec(ctx, clearForkCacheQuery, s.JID, kind)
	return err
}

const (
	getSenderKeyDevicesQuery       = `SELECT device FROM whatsmeow_fork_sender_key_devices WHERE our_jid=$1 AND group_jid=$2 AND fingerprint=$3`
	dropStaleSenderKeyDevicesQuery = `DELETE FROM whatsmeow_fork_sender_key_devices WHERE our_jid=$1 AND group_jid=$2 AND fingerprint<>$3`
	putSenderKeyDeviceQuery        = `
		INSERT INTO whatsmeow_fork_sender_key_devices (our_jid, group_jid, device, user_key, fingerprint) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (our_jid, group_jid, device) DO UPDATE SET fingerprint=excluded.fingerprint
	`
	forgetSenderKeyDeviceQuery        = `DELETE FROM whatsmeow_fork_sender_key_devices WHERE our_jid=$1 AND device=$2`
	forgetSenderKeyDeviceInGroupQuery = `DELETE FROM whatsmeow_fork_sender_key_devices WHERE our_jid=$1 AND group_jid=$2 AND device=$3`
	forgetSenderKeyUserQuery          = `DELETE FROM whatsmeow_fork_sender_key_devices WHERE our_jid=$1 AND user_key=$2`
)

// GetSenderKeyDevices returns the devices of a group recorded as holding the
// sender key with the given fingerprint.
func (s *SQLStore) GetSenderKeyDevices(ctx context.Context, group, fingerprint string) ([]string, error) {
	rows, err := s.db.Query(ctx, getSenderKeyDevicesQuery, s.JID, group, fingerprint)
	return dbutil.NewRowIterWithError(rows, dbutil.ScanSingleColumn[string], err).AsList()
}

// PutSenderKeyDevices records devices as holding the group's sender key with
// the given fingerprint, and drops records for any older key of the group.
// userKeys maps each device to its user (see ForgetSenderKeyUser).
func (s *SQLStore) PutSenderKeyDevices(ctx context.Context, group, fingerprint string, userKeys map[string]string) error {
	return s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		if _, err := s.db.Exec(ctx, dropStaleSenderKeyDevicesQuery, s.JID, group, fingerprint); err != nil {
			return err
		}
		for device, user := range userKeys {
			if _, err := s.db.Exec(ctx, putSenderKeyDeviceQuery, s.JID, group, device, user, fingerprint); err != nil {
				return err
			}
		}
		return nil
	})
}

// ForgetSenderKeyDevice drops the records of a device, in one group or (with
// an empty group) in all of them.
func (s *SQLStore) ForgetSenderKeyDevice(ctx context.Context, group, device string) error {
	var err error
	if group == "" {
		_, err = s.db.Exec(ctx, forgetSenderKeyDeviceQuery, s.JID, device)
	} else {
		_, err = s.db.Exec(ctx, forgetSenderKeyDeviceInGroupQuery, s.JID, group, device)
	}
	return err
}

// ForgetSenderKeyUser drops the records of every device of a user, in every
// group.
func (s *SQLStore) ForgetSenderKeyUser(ctx context.Context, user string) error {
	_, err := s.db.Exec(ctx, forgetSenderKeyUserQuery, s.JID, user)
	return err
}

// GetTCTokenIssued returns when our privacy token was last issued to the
// given user, or the zero time if it never was.
func (s *SQLStore) GetTCTokenIssued(ctx context.Context, user types.JID) (time.Time, error) {
	var ts int64
	err := s.db.QueryRow(ctx, getTCTokenIssuedQuery, s.JID, user.ToNonAD().String()).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, err
	}
	return time.Unix(ts, 0), nil
}

// PutTCTokenIssued records that our privacy token was issued to the user.
func (s *SQLStore) PutTCTokenIssued(ctx context.Context, user types.JID, ts time.Time) error {
	_, err := s.db.Exec(ctx, putTCTokenIssuedQuery, s.JID, user.ToNonAD().String(), ts.Unix())
	return err
}

// DeleteExpiredTCTokenIssued forgets issuances older than the cutoff.
func (s *SQLStore) DeleteExpiredTCTokenIssued(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.Exec(ctx, deleteExpiredTCTokenIssuedQuery, s.JID, cutoff.Unix())
	return err
}

func (s *SQLStore) DeleteExpiredPrivacyTokens(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(ctx, deleteExpiredPrivacyTokens, s.JID, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

const (
	getBufferedEventQuery = `
		SELECT plaintext, server_timestamp, insert_timestamp FROM whatsmeow_event_buffer WHERE our_jid = $1 AND ciphertext_hash = $2
	`
	putBufferedEventQuery = `
		INSERT INTO whatsmeow_event_buffer (our_jid, ciphertext_hash, plaintext, server_timestamp, insert_timestamp)
		VALUES ($1, $2, $3, $4, $5)
	`
	clearBufferedEventPlaintextQuery = `
		UPDATE whatsmeow_event_buffer SET plaintext = NULL WHERE our_jid = $1 AND ciphertext_hash = $2
	`
	deleteOldBufferedHashesQuery = `
		DELETE FROM whatsmeow_event_buffer WHERE insert_timestamp < $1
	`
)

func (s *SQLStore) GetBufferedEvent(ctx context.Context, ciphertextHash [32]byte) (*store.BufferedEvent, error) {
	var insertTimeMS, serverTimeSeconds int64
	var buf store.BufferedEvent
	err := s.db.QueryRow(ctx, getBufferedEventQuery, s.JID, ciphertextHash[:]).Scan(&buf.Plaintext, &serverTimeSeconds, &insertTimeMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	buf.ServerTime = time.Unix(serverTimeSeconds, 0)
	buf.InsertTime = time.UnixMilli(insertTimeMS)
	return &buf, nil
}

func (s *SQLStore) PutBufferedEvent(ctx context.Context, ciphertextHash [32]byte, plaintext []byte, serverTimestamp time.Time) error {
	_, err := s.db.Exec(ctx, putBufferedEventQuery, s.JID, ciphertextHash[:], plaintext, serverTimestamp.Unix(), time.Now().UnixMilli())
	return err
}

func (s *SQLStore) DoDecryptionTxn(ctx context.Context, fn func(context.Context) error) error {
	ctx = context.WithValue(ctx, dbutil.ContextKeyDoTxnCallerSkip, 2)
	return s.db.DoTxn(ctx, nil, fn)
}

func (s *SQLStore) ClearBufferedEventPlaintext(ctx context.Context, ciphertextHash [32]byte) error {
	_, err := s.db.Exec(ctx, clearBufferedEventPlaintextQuery, s.JID, ciphertextHash[:])
	return err
}

func (s *SQLStore) DeleteOldBufferedHashes(ctx context.Context) error {
	// The WhatsApp servers only buffer events for 14 days,
	// so we can safely delete anything older than that.
	_, err := s.db.Exec(ctx, deleteOldBufferedHashesQuery, time.Now().Add(-14*24*time.Hour).UnixMilli())
	return err
}

const (
	getOutgoingEventQuery = `
		SELECT format, plaintext FROM whatsmeow_retry_buffer WHERE our_jid=$1 AND (chat_jid=$2 OR chat_jid=$3) AND message_id=$4
	`
	addOutgoingEventQuery = `
		INSERT INTO whatsmeow_retry_buffer (our_jid, chat_jid, message_id, format, plaintext, timestamp)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (our_jid, chat_jid, message_id) DO UPDATE
			SET format=excluded.format, plaintext=excluded.plaintext, timestamp=excluded.timestamp
	`
	deleteOldOutgoingEventsQuery = `
		DELETE FROM whatsmeow_retry_buffer WHERE our_jid=$1 AND timestamp < $2
	`
)

func (s *SQLStore) GetOutgoingEvent(ctx context.Context, chatJID, altChatJID types.JID, id types.MessageID) (format string, result []byte, err error) {
	err = s.db.QueryRow(ctx, getOutgoingEventQuery, s.JID, chatJID, altChatJID, id).Scan(&format, &result)
	return
}

func (s *SQLStore) AddOutgoingEvent(ctx context.Context, chatJID types.JID, id types.MessageID, format string, plaintext []byte) error {
	_, err := s.db.Exec(ctx, addOutgoingEventQuery, s.JID, chatJID, id, format, plaintext, time.Now().UnixMilli())
	return err
}

func (s *SQLStore) DeleteOldOutgoingEvents(ctx context.Context) error {
	_, err := s.db.Exec(ctx, deleteOldOutgoingEventsQuery, s.JID, time.Now().Add(-7*24*time.Hour).UnixMilli())
	return err
}
