package whatsmeow

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func dispatchLock(t *testing.T, chat string, act *waSyncAction.LockChatAction) any {
	t.Helper()
	cli := &Client{Log: waLog.Noop, Store: &store.Device{
		ID: &types.JID{User: "5511000000001", Server: types.DefaultUserServer},
	}}
	return cli.dispatchAppState(context.Background(), appstate.WAPatchRegularHigh, appstate.Mutation{
		Operation: waServerSync.SyncdMutation_SET,
		Index:     []string{appstate.IndexLock, chat},
		Action: &waSyncAction.SyncActionValue{
			Timestamp:      proto.Int64(1_700_000_000_000),
			LockChatAction: act,
		},
	}, false)
}

func TestLockMutationWithoutLockedIsDropped(t *testing.T) {
	for _, chat := range []string{"12025550111@s.whatsapp.net", "100000012345678@lid"} {
		if evt := dispatchLock(t, chat, &waSyncAction.LockChatAction{}); evt != nil {
			t.Errorf("%s: lock without locked dispatched %#v", chat, evt)
		}
		if evt := dispatchLock(t, chat, nil); evt != nil {
			t.Errorf("%s: lock without action dispatched %#v", chat, evt)
		}
	}
}

func TestLockMutationDispatchesBothStates(t *testing.T) {
	for _, locked := range []bool{true, false} {
		evt, ok := dispatchLock(t, "12025550111@s.whatsapp.net", &waSyncAction.LockChatAction{Locked: proto.Bool(locked)}).(*events.LockChat)
		if !ok {
			t.Fatalf("locked=%v: no LockChat event", locked)
		}
		if evt.Action.GetLocked() != locked {
			t.Errorf("locked=%v: event says %v", locked, evt.Action.GetLocked())
		}
	}
}
