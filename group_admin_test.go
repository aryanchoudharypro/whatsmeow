package whatsmeow

import (
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestParseReportedGroupMessages(t *testing.T) {
	reporter := types.NewJID("111", types.HiddenUserServer)
	resp := &waBinary.Node{Tag: "iq", Content: []waBinary.Node{{
		Tag: "reports",
		Content: []waBinary.Node{{
			Tag:   "report",
			Attrs: waBinary.Attrs{"message_id": "M1"},
			Content: []waBinary.Node{{
				Tag:   "reporter",
				Attrs: waBinary.Attrs{"jid": reporter, "timestamp": "1700000000", "username": "sam"},
			}},
		}},
	}}}
	reports, err := parseReportedGroupMessages(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].MessageID != "M1" || len(reports[0].Reporters) != 1 {
		t.Fatalf("reports = %+v", reports)
	}
	r := reports[0].Reporters[0]
	if r.JID != reporter || r.Username != "sam" || r.Timestamp.Unix() != 1700000000 || !r.PhoneNumber.IsEmpty() {
		t.Fatalf("reporter = %+v", r)
	}
	if _, err := parseReportedGroupMessages(&waBinary.Node{Tag: "iq"}); err == nil {
		t.Fatal("a response without <reports> parsed")
	}
}

func TestBuildGroupMemberLabel(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	pm := cli.BuildGroupMemberLabel("VIP").GetProtocolMessage()
	if pm.GetType() != waE2E.ProtocolMessage_GROUP_MEMBER_LABEL_CHANGE || pm.GetMemberLabel().GetLabel() != "VIP" ||
		pm.GetMemberLabel().GetLabelTimestamp() == 0 || pm.GetKey() != nil {
		t.Fatalf("label message = %v", pm)
	}
}

func TestParseGroupModerationSettings(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	node := &waBinary.Node{Tag: "group", Attrs: waBinary.Attrs{"id": "123"}, Content: []waBinary.Node{
		{Tag: "no_frequently_forwarded"},
		{Tag: "allow_admin_reports"},
		{Tag: "group_history"},
	}}
	info, err := cli.parseGroupNode(node)
	if err != nil {
		t.Fatal(err)
	}
	if !info.NoFrequentlyForwarded || !info.AllowAdminReports || !info.HasGroupHistory {
		t.Fatalf("moderation = %+v", info.GroupModeration)
	}
	info, err = cli.parseGroupNode(&waBinary.Node{Tag: "group", Attrs: waBinary.Attrs{"id": "123"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.GroupModeration != (types.GroupModeration{}) {
		t.Fatalf("settings that weren't sent must read as off: %+v", info.GroupModeration)
	}
}
