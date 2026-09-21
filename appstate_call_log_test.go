package whatsmeow

import (
	"context"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func callLogMutation(index []string, record *waSyncAction.CallLogRecord) appstate.Mutation {
	return appstate.Mutation{
		Operation: waServerSync.SyncdMutation_SET,
		Index:     index,
		Action: &waSyncAction.SyncActionValue{
			Timestamp: proto.Int64(1_700_000_000_000),
			CallLogAction: &waSyncAction.CallLogAction{
				CallLogRecord: record,
			},
		},
	}
}

// The shape WA Web sends: ["call_log", callCreatorJID, callID, fromMe].
func fullCallLogIndex() []string {
	return []string{appstate.IndexCallLog, "5511999990000@s.whatsapp.net", "call-42", "1"}
}

const (
	ownPN  = "5511000000001"
	ownLID = "9988776655443"
	peerPN = "5511999990000"
)

func dispatchCallLog(t *testing.T, mutation appstate.Mutation) any {
	t.Helper()
	device := &store.Device{
		ID:  &types.JID{User: ownPN, Server: types.DefaultUserServer, Device: 0},
		LID: types.JID{User: ownLID, Server: types.HiddenUserServer},
	}
	cli := &Client{Log: waLog.Noop, Store: device}
	return cli.dispatchAppState(context.Background(), appstate.WAPatchRegular, mutation, false)
}

func TestCallLogMutationDispatchesEvent(t *testing.T) {
	record := &waSyncAction.CallLogRecord{
		CallResult: waSyncAction.CallLogRecord_CONNECTED.Enum(),
		Duration:   proto.Int64(125),
		StartTime:  proto.Int64(1_700_000_000),
		IsVideo:    proto.Bool(true),
		// Deliberately the opposite of FromMe: WA Web writes this field as
		// isIncoming = fromMe, so a consumer reading it at face value files every
		// call backwards. The event must not take it.
		IsIncoming: proto.Bool(false),
	}
	index := fullCallLogIndex()
	index[1] = ownPN + "@" + types.DefaultUserServer
	evt, ok := dispatchCallLog(t, callLogMutation(index, record)).(*events.CallLogSync)
	if !ok {
		t.Fatal("call_log mutation dispatched no CallLogSync event")
	}
	if !evt.FromMe {
		t.Error("a call whose creator is our own account is outbound, whatever the direction fields say")
	}
	if evt.CallID != "call-42" {
		t.Errorf("CallID = %q, want call-42", evt.CallID)
	}
	if evt.CallCreatorJID.User != ownPN {
		t.Errorf("CallCreatorJID = %v, want the index's JID", evt.CallCreatorJID)
	}
	if evt.Record.GetDuration() != 125 || !evt.Record.GetIsVideo() {
		t.Errorf("record not carried through: duration=%d video=%v", evt.Record.GetDuration(), evt.Record.GetIsVideo())
	}
	if evt.Record.GetCallResult() != waSyncAction.CallLogRecord_CONNECTED {
		t.Errorf("CallResult = %v, want CONNECTED", evt.Record.GetCallResult())
	}
}

func TestCallLogDirectionComesFromTheCreator(t *testing.T) {
	cases := []struct {
		name     string
		creator  string
		fourth   string
		incoming bool
		wantMine bool
	}{
		// The reported bug: a call placed from the account's own handset, whose
		// index and record both claim it was not from us.
		{"own PN creator, both fields say otherwise", ownPN + "@" + types.DefaultUserServer, "0", true, true},
		{"own LID creator", ownLID + "@" + types.HiddenUserServer, "0", true, true},
		{"peer creator, both fields claim it was ours", peerPN + "@" + types.DefaultUserServer, "1", false, false},
		// A peer LID whose digits spell our phone number is NOT us: the
		// comparison is keyed on the addressing mode, never across it.
		{"peer LID spelling our phone number", ownPN + "@" + types.HiddenUserServer, "1", false, false},
		{"our LID digits on the PN server", ownLID + "@" + types.DefaultUserServer, "1", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := fullCallLogIndex()
			index[1] = tc.creator
			index[3] = tc.fourth
			record := &waSyncAction.CallLogRecord{IsIncoming: proto.Bool(tc.incoming)}
			evt, ok := dispatchCallLog(t, callLogMutation(index, record)).(*events.CallLogSync)
			if !ok {
				t.Fatal("call_log mutation dispatched no CallLogSync event")
			}
			if evt.FromMe != tc.wantMine {
				t.Errorf("FromMe = %v, want %v (creator %s)", evt.FromMe, tc.wantMine, tc.creator)
			}
		})
	}
}

// A shape we cannot read is skipped rather than guessed at, because guessing
// mislabels the call in the one direction the user would notice.
func TestCallLogMutationSkipsUnreadableShapes(t *testing.T) {
	short := fullCallLogIndex()[:2]
	if evt := dispatchCallLog(t, callLogMutation(short, &waSyncAction.CallLogRecord{})); evt != nil {
		t.Errorf("a 2-part index has no call id and must dispatch nothing, got %T", evt)
	}

	// Nothing reads the fourth part any more, so an odd one is no longer a
	// reason to drop a record whose direction we derive elsewhere.
	oddFourth := fullCallLogIndex()
	oddFourth[3] = "true"
	if evt := dispatchCallLog(t, callLogMutation(oddFourth, &waSyncAction.CallLogRecord{})); evt == nil {
		t.Error("an unread fourth index part must no longer drop the mutation")
	}

	noCallID := fullCallLogIndex()
	noCallID[2] = ""
	if evt := dispatchCallLog(t, callLogMutation(noCallID, &waSyncAction.CallLogRecord{})); evt != nil {
		t.Errorf("a missing call id must dispatch nothing, got %T", evt)
	}

	if evt := dispatchCallLog(t, callLogMutation(fullCallLogIndex(), nil)); evt != nil {
		t.Errorf("a missing record must dispatch nothing, got %T", evt)
	}
}
