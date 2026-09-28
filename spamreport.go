package whatsmeow

import (
	"context"
	"strconv"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// SpamFlow is where in the app a spam report was made from.
type SpamFlow string

const (
	SpamFlowMessageMenu           SpamFlow = "MessageMenu"
	SpamFlowContactInfo           SpamFlow = "ContactInfo"
	SpamFlowGroupInfoReport       SpamFlow = "GroupInfoReport"
	SpamFlowGroupSpamBannerReport SpamFlow = "GroupSpamBannerReport"
	SpamFlowStatusReport          SpamFlow = "StatusReport"
)

// SpamReport describes one message reported as spam.
type SpamReport struct {
	MessageID        types.MessageID
	MessageTimestamp int64
	// From is who sent the message (the chat for a 1:1 chat).
	From types.JID
	// Participant is the sender inside a group.
	Participant types.JID
	// Group and GroupSubject identify the group, for group reports.
	Group        types.JID
	GroupSubject string
	Flow         SpamFlow
	// RawMessage is the reported message's protobuf, as WhatsApp Web sends it.
	RawMessage       []byte
	MediaType        string
	LocalMessageType string
}

func buildSpamListNode(report SpamReport) waBinary.Node {
	messageAttrs := waBinary.Attrs{
		"id": report.MessageID,
		"t":  strconv.FormatInt(report.MessageTimestamp, 10),
	}
	if !report.From.IsEmpty() {
		messageAttrs["from"] = report.From
	}
	if !report.Participant.IsEmpty() {
		messageAttrs["participant"] = report.Participant
	}
	message := waBinary.Node{Tag: "message", Attrs: messageAttrs}
	if len(report.RawMessage) > 0 {
		rawAttrs := waBinary.Attrs{"v": "3"}
		if report.MediaType != "" {
			rawAttrs["mediatype"] = report.MediaType
		}
		if report.LocalMessageType != "" {
			rawAttrs["local_message_type"] = report.LocalMessageType
		}
		message.Content = []waBinary.Node{{Tag: "raw", Attrs: rawAttrs, Content: report.RawMessage}}
	}
	flow := report.Flow
	if flow == "" {
		flow = SpamFlowMessageMenu
	}
	listAttrs := waBinary.Attrs{"spam_flow": string(flow)}
	if !report.Group.IsEmpty() {
		listAttrs["jid"] = report.Group
	}
	if report.GroupSubject != "" {
		listAttrs["subject"] = report.GroupSubject
	}
	return waBinary.Node{Tag: "spam_list", Attrs: listAttrs, Content: []waBinary.Node{message}}
}

// SendSpamReport reports a message as spam to WhatsApp, the way the official
// apps' "Report" does, and returns the server's report ID if it gave one.
// Blocking the sender is separate (see UpdateBlocklist).
//
// Ported from whatsapp-rust's send_spam_report.
func (cli *Client) SendSpamReport(ctx context.Context, report SpamReport) (string, error) {
	content := []waBinary.Node{buildSpamListNode(report)}
	// A privacy-restricted account only accepts the report with the reported
	// contact's trusted-contact token, where the server has that turned on.
	if !report.From.IsEmpty() {
		if props, err := cli.GetABProps(ctx); err == nil && props.Bool(ABPropSpamReportWithPrivacyToken, false) {
			if token, _ := cli.ensureTCToken(ctx, report.From); len(token) > 0 {
				content = append(content, waBinary.Node{Tag: "tctoken", Content: token})
			}
		}
	}
	resp, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "spam",
		Type:      iqSet,
		To:        types.ServerJID,
		Content:   content,
	})
	if err != nil {
		return "", err
	}
	if reportID, ok := resp.GetOptionalChildByTag("report_id"); ok {
		switch id := reportID.Content.(type) {
		case string:
			return id, nil
		case []byte:
			return string(id), nil
		}
	}
	return "", nil
}
