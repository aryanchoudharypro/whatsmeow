package appstate

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// memAppState keeps one collection's version, hash and value MACs, and one key.
type memAppState struct {
	store.AppStateStore
	store.AppStateSyncKeyStore
	version uint64
	hash    [128]byte
	macs    map[string][]byte
}

func (m *memAppState) PutAppStateVersion(_ context.Context, _ string, version uint64, hash [128]byte) error {
	m.version, m.hash = version, hash
	return nil
}

func (m *memAppState) PutAppStateMutationMACs(_ context.Context, _ string, _ uint64, mutations []store.AppStateMutationMAC) error {
	for _, mut := range mutations {
		m.macs[string(mut.IndexMAC)] = mut.ValueMAC
	}
	return nil
}

func (m *memAppState) DeleteAppStateMutationMACs(_ context.Context, _ string, indexMACs [][]byte) error {
	for _, index := range indexMACs {
		delete(m.macs, string(index))
	}
	return nil
}

func (m *memAppState) GetAppStateMutationMAC(_ context.Context, _ string, indexMAC []byte) ([]byte, error) {
	return m.macs[string(indexMAC)], nil
}

func (m *memAppState) GetAppStateSyncKey(_ context.Context, _ []byte) (*store.AppStateSyncKey, error) {
	return &store.AppStateSyncKey{Data: make([]byte, 32)}, nil
}

// roundTrip encodes a patch at the stored state and applies it back, as
// sending one and fetching it again does.
func roundTrip(t *testing.T, proc *Processor, mem *memAppState, patch PatchInfo) ([]Mutation, error) {
	t.Helper()
	ctx := context.Background()
	state := HashState{Version: mem.version, Hash: mem.hash}
	encoded, err := proc.EncodePatch(ctx, []byte("key"), state, patch)
	if err != nil {
		return nil, err
	}
	var parsed waServerSync.SyncdPatch
	if err = proto.Unmarshal(encoded, &parsed); err != nil {
		t.Fatal(err)
	}
	parsed.Version = &waServerSync.SyncdVersion{Version: proto.Uint64(state.Version + 1)}
	muts, _, err := proc.DecodePatches(ctx, &PatchList{Name: patch.Type, Patches: []*waServerSync.SyncdPatch{&parsed}}, state, true)
	if err != nil {
		t.Fatalf("decoding our own patch: %v", err)
	}
	return muts, nil
}

func TestRemoveContactUndoesTheSave(t *testing.T) {
	mem := &memAppState{macs: map[string][]byte{}}
	proc := NewProcessor(&store.Device{AppState: mem, AppStateKeys: mem}, waLog.Noop)
	pn := types.NewJID("15550001111", types.DefaultUserServer)
	lid := types.NewJID("12345", types.HiddenUserServer)

	saved, err := roundTrip(t, proc, mem, PatchInfo{Type: WAPatchCriticalUnblockLow, Mutations: []MutationInfo{{
		Index:   []string{IndexContact, pn.String()},
		Version: 2,
		Value:   &waSyncAction.SyncActionValue{ContactAction: &waSyncAction.ContactAction{FullName: proto.String("Alex")}},
	}}})
	if err != nil || len(saved) != 1 || saved[0].Operation != waServerSync.SyncdMutation_SET {
		t.Fatalf("save: %v %+v", err, saved)
	}

	// Only the phone number form was saved, so only it is removed.
	removed, err := roundTrip(t, proc, mem, BuildRemoveContact(pn, lid))
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Operation != waServerSync.SyncdMutation_REMOVE || removed[0].Index[1] != pn.String() {
		t.Fatalf("remove: %+v", removed)
	}
	if mem.hash != [128]byte{} {
		t.Error("the collection's hash should be back to empty once its one record is removed")
	}
	if len(mem.macs) != 0 {
		t.Error("the removed record's MAC is still stored")
	}

	if _, err = roundTrip(t, proc, mem, BuildRemoveContact(pn)); !errors.Is(err, ErrNothingToSend) {
		t.Errorf("removing a contact that isn't saved: got %v, want ErrNothingToSend", err)
	}
}
