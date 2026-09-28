package whatsmeow

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// Group settings and admin tools from WhatsApp's newer group features,
// ported from whatsapp-rust's Groups.

func (cli *Client) setGroupToggle(ctx context.Context, jid types.JID, on bool, onTag, offTag string) error {
	tag := offTag
	if on {
		tag = onTag
	}
	_, err := cli.sendGroupIQ(ctx, iqSet, jid, waBinary.Node{Tag: tag})
	return err
}

// SetGroupNoFrequentlyForwarded restricts (or allows again) messages that
// have been forwarded many times in the group.
func (cli *Client) SetGroupNoFrequentlyForwarded(ctx context.Context, jid types.JID, restrict bool) error {
	return cli.setGroupToggle(ctx, jid, restrict, "no_frequently_forwarded", "frequently_forwarded_ok")
}

// SetGroupAllowAdminReports lets members report messages to the group's
// admins (see ReportGroupMessagesToAdmins).
func (cli *Client) SetGroupAllowAdminReports(ctx context.Context, jid types.JID, allow bool) error {
	return cli.setGroupToggle(ctx, jid, allow, "allow_admin_reports", "not_allow_admin_reports")
}

// SetGroupHistorySharing turns on or off sharing recent history with people
// added to the group.
func (cli *Client) SetGroupHistorySharing(ctx context.Context, jid types.JID, enabled bool) error {
	return cli.setGroupToggle(ctx, jid, enabled, "group_history", "no_group_history")
}

// ReportGroupMessagesToAdmins reports messages to the group's own admins (not
// to WhatsApp). The group must allow admin reports for the server to accept it.
func (cli *Client) ReportGroupMessagesToAdmins(ctx context.Context, jid types.JID, messageIDs []types.MessageID) error {
	if len(messageIDs) == 0 {
		return fmt.Errorf("no messages to report")
	}
	reports := make([]waBinary.Node, len(messageIDs))
	for i, id := range messageIDs {
		reports[i] = waBinary.Node{Tag: "report", Attrs: waBinary.Attrs{"message_id": id}}
	}
	_, err := cli.sendGroupIQ(ctx, iqSet, jid, waBinary.Node{Tag: "reports", Content: reports})
	return err
}

// GroupMessageReporter is one member who reported a message to the admins.
type GroupMessageReporter struct {
	JID         types.JID
	PhoneNumber types.JID
	Username    string
	Timestamp   time.Time
}

// ReportedGroupMessage is a message reported to a group's admins, and by whom.
type ReportedGroupMessage struct {
	MessageID types.MessageID
	Reporters []GroupMessageReporter
}

// GetReportedGroupMessages lists the messages members have reported to this
// group's admins.
func (cli *Client) GetReportedGroupMessages(ctx context.Context, jid types.JID) ([]ReportedGroupMessage, error) {
	resp, err := cli.sendGroupIQ(ctx, iqGet, jid, waBinary.Node{Tag: "reports"})
	if err != nil {
		return nil, err
	}
	return parseReportedGroupMessages(resp)
}

func parseReportedGroupMessages(resp *waBinary.Node) ([]ReportedGroupMessage, error) {
	reportsNode, ok := resp.GetOptionalChildByTag("reports")
	if !ok {
		return nil, &ElementMissingError{Tag: "reports", In: "reported group messages response"}
	}
	var out []ReportedGroupMessage
	for _, report := range reportsNode.GetChildrenByTag("report") {
		ag := report.AttrGetter()
		msg := ReportedGroupMessage{MessageID: ag.String("message_id")}
		for _, reporter := range report.GetChildrenByTag("reporter") {
			rag := reporter.AttrGetter()
			msg.Reporters = append(msg.Reporters, GroupMessageReporter{
				JID:         rag.JID("jid"),
				PhoneNumber: rag.OptionalJIDOrEmpty("phone_number"),
				Username:    rag.OptionalString("username"),
				Timestamp:   rag.UnixTime("timestamp"),
			})
			if !rag.OK() {
				return nil, fmt.Errorf("failed to parse reporter of %s: %w", msg.MessageID, rag.Error())
			}
		}
		if !ag.OK() {
			return nil, fmt.Errorf("failed to parse reported message: %w", ag.Error())
		}
		out = append(out, msg)
	}
	return out, nil
}

// BuildGroupMemberLabel builds the message that sets (or, with an empty
// label, clears) our own member label in a group - the short tag shown next
// to a member's name. Send it to the group with Client.SendMessage.
func (cli *Client) BuildGroupMemberLabel(label string) *waE2E.Message {
	return &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_GROUP_MEMBER_LABEL_CHANGE.Enum(),
			MemberLabel: &waE2E.MemberLabel{
				Label:          proto.String(label),
				LabelTimestamp: proto.Int64(time.Now().Unix()),
			},
		},
	}
}

// groupNodeText is a group setting element's text, which arrives as bytes or,
// when it's a dictionary token, as a string.
func groupNodeText(node *waBinary.Node) string {
	switch content := node.Content.(type) {
	case []byte:
		return string(content)
	case string:
		return content
	}
	return ""
}
