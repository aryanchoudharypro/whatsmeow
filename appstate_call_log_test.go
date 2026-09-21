package whatsmeow

import (
	"context"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
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

func dispatchCallLog(t *testing.T, mutation appstate.Mutation) any {
	t.Helper()
	cli := &Client{Log: waLog.Noop, Store: &store.Device{}}
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
	evt, ok := dispatchCallLog(t, callLogMutation(fullCallLogIndex(), record)).(*events.CallLogSync)
	if !ok {
		t.Fatal("call_log mutation dispatched no CallLogSync event")
	}
	if !evt.FromMe {
		t.Error("FromMe must come from index[3] == \"1\", not from the record's IsIncoming")
	}
	if evt.CallID != "call-42" {
		t.Errorf("CallID = %q, want call-42", evt.CallID)
	}
	if evt.CallCreatorJID.User != "5511999990000" {
		t.Errorf("CallCreatorJID = %v, want the index's JID", evt.CallCreatorJID)
	}
	if evt.Record.GetDuration() != 125 || !evt.Record.GetIsVideo() {
		t.Errorf("record not carried through: duration=%d video=%v", evt.Record.GetDuration(), evt.Record.GetIsVideo())
	}
	if evt.Record.GetCallResult() != waSyncAction.CallLogRecord_CONNECTED {
		t.Errorf("CallResult = %v, want CONNECTED", evt.Record.GetCallResult())
	}
}

func TestCallLogMutationFromMeFalse(t *testing.T) {
	index := fullCallLogIndex()
	index[3] = "0"
	evt, ok := dispatchCallLog(t, callLogMutation(index, &waSyncAction.CallLogRecord{})).(*events.CallLogSync)
	if !ok {
		t.Fatal("call_log mutation dispatched no CallLogSync event")
	}
	if evt.FromMe {
		t.Error("index[3] == \"0\" must read as an incoming call")
	}
}

// A shape we cannot read is skipped rather than guessed at, because guessing
// mislabels the call in the one direction the user would notice.
func TestCallLogMutationSkipsUnreadableShapes(t *testing.T) {
	short := fullCallLogIndex()[:3]
	if evt := dispatchCallLog(t, callLogMutation(short, &waSyncAction.CallLogRecord{})); evt != nil {
		t.Errorf("a 3-part index must dispatch nothing, got %T", evt)
	}

	badFromMe := fullCallLogIndex()
	badFromMe[3] = "true"
	if evt := dispatchCallLog(t, callLogMutation(badFromMe, &waSyncAction.CallLogRecord{})); evt != nil {
		t.Errorf("an unreadable fromMe must dispatch nothing, got %T", evt)
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
