// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	groupRecord "go.mau.fi/libsignal/groups/state/record"
	"go.mau.fi/libsignal/protocol"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const InviteLinkPrefix = "https://chat.whatsapp.com/"

func (cli *Client) sendGroupIQ(ctx context.Context, iqType infoQueryType, jid types.JID, content waBinary.Node) (*waBinary.Node, error) {
	return cli.sendIQ(ctx, infoQuery{
		Namespace: "w:g2",
		Type:      iqType,
		To:        jid,
		Content:   []waBinary.Node{content},
	})
}

// ReqCreateGroup contains the request data for CreateGroup.
type ReqCreateGroup struct {
	// Group names are limited to 25 characters. A longer group name will cause a 406 not acceptable error.
	Name string
	// You don't need to include your own JID in the participants array, the WhatsApp servers will add it implicitly.
	Participants []types.JID

	types.GroupEphemeral
	types.GroupAnnounce
	types.GroupLocked
	types.GroupMembershipApprovalMode
	MemberAddMode types.GroupMemberAddMode
	// Set IsParent to true to create a community instead of a normal group.
	// When creating a community, the linked announcement group will be created automatically by the server.
	types.GroupParent
	// Community only: also create a general chat that every member joins.
	CreateGeneralChat bool
	// Set LinkedParentJID to create a group inside a community.
	types.GroupLinkedParent
	// Community group only: keep the group out of the community's group list
	// for people who aren't in it. This can't be changed after creation.
	IsHiddenGroup bool
}

// CreateGroup creates a group on WhatsApp with the given name and participants.
//
// See ReqCreateGroup for parameters.
func (cli *Client) CreateGroup(ctx context.Context, req ReqCreateGroup) (*types.GroupInfo, error) {
	participantNodes := make([]waBinary.Node, len(req.Participants), len(req.Participants)+1)
	// TODO member_share_group_history_mode
	participantNodes = append(participantNodes, waBinary.Node{
		Tag:     "member_add_mode",
		Content: string(cmp.Or(req.MemberAddMode, types.GroupMemberAddModeAllMember)),
	})
	for i, participant := range req.Participants {
		participant = participant.ToNonAD()
		var participantPN types.JID
		if participant.Server == types.HiddenUserServer {
			var err error
			participantPN, err = cli.Store.LIDs.GetPNForLID(ctx, participant)
			if err != nil {
				return nil, fmt.Errorf("failed to get phone number for participant %s: %v", participant, err)
			}
		}
		participantAttrs := waBinary.Attrs{"jid": participant}
		if !participantPN.IsEmpty() {
			participantAttrs["phone_number"] = participantPN
		}
		participantNodes[i] = waBinary.Node{
			Tag:   "participant",
			Attrs: participantAttrs,
		}
		token, err := cli.ensureTCToken(ctx, participant)
		if err != nil {
			return nil, fmt.Errorf("failed to get privacy token for participant %s: %v", participant, err)
		} else if len(token) > 0 {
			participantNodes[i].Content = []waBinary.Node{{
				Tag:     "privacy",
				Content: token,
			}}
		}
	}
	if req.IsParent {
		if req.DefaultMembershipApprovalMode == "" {
			req.DefaultMembershipApprovalMode = "request_required"
		}
		participantNodes = append(participantNodes, waBinary.Node{
			Tag: "parent",
			Attrs: waBinary.Attrs{
				"default_membership_approval_mode": req.DefaultMembershipApprovalMode,
			},
		})
		if req.AllowNonAdminSubGroupCreation {
			participantNodes = append(participantNodes, waBinary.Node{Tag: "allow_non_admin_sub_group_creation"})
		}
		if req.CreateGeneralChat {
			participantNodes = append(participantNodes, waBinary.Node{Tag: "create_general_chat"})
		}
	} else if !req.LinkedParentJID.IsEmpty() {
		participantNodes = append(participantNodes, waBinary.Node{
			Tag:   "linked_parent",
			Attrs: waBinary.Attrs{"jid": req.LinkedParentJID},
		})
	}
	if req.IsHiddenGroup {
		participantNodes = append(participantNodes, waBinary.Node{Tag: "hidden_group"})
	}
	if req.IsLocked {
		participantNodes = append(participantNodes, waBinary.Node{Tag: "locked"})
	}
	if req.IsAnnounce {
		participantNodes = append(participantNodes, waBinary.Node{Tag: "announcement"})
	}
	if req.IsEphemeral {
		participantNodes = append(participantNodes, waBinary.Node{
			Tag: "ephemeral",
			Attrs: waBinary.Attrs{
				"expiration": req.DisappearingTimer,
				"trigger":    "1", // TODO what's this?
			},
		})
	} else {
		participantNodes = append(participantNodes, waBinary.Node{
			Tag:   "ephemeral",
			Attrs: waBinary.Attrs{"expiration": 0},
		})
	}
	approvalState := "off"
	if req.IsJoinApprovalRequired {
		approvalState = "on"
	}
	participantNodes = append(participantNodes, waBinary.Node{
		Tag: "membership_approval_mode",
		Content: []waBinary.Node{{
			Tag:   "group_join",
			Attrs: waBinary.Attrs{"state": approvalState},
		}},
	})
	createAttrs := waBinary.Attrs{}
	if req.Name != "" {
		createAttrs["subject"] = req.Name
	}
	resp, err := cli.sendGroupIQ(ctx, iqSet, types.GroupServerJID, waBinary.Node{
		Tag:     "create",
		Attrs:   createAttrs,
		Content: participantNodes,
	})
	if err != nil {
		return nil, err
	}
	groupNode, ok := groupOrCommunityChild(resp)
	if !ok {
		return nil, &ElementMissingError{Tag: "group", In: "response to create group query"}
	}
	return cli.parseGroupNode(&groupNode)
}

// UnlinkGroup removes a child group from a parent community.
func (cli *Client) UnlinkGroup(ctx context.Context, parent, child types.JID) error {
	_, err := cli.sendGroupIQ(ctx, iqSet, parent, waBinary.Node{
		Tag:   "unlink",
		Attrs: waBinary.Attrs{"unlink_type": string(types.GroupLinkChangeTypeSub)},
		Content: []waBinary.Node{{
			Tag:   "group",
			Attrs: waBinary.Attrs{"jid": child},
		}},
	})
	return err
}

// LinkGroup adds an existing group as a child group in a community.
//
// To create a new group within a community, set LinkedParentJID in the CreateGroup request.
func (cli *Client) LinkGroup(ctx context.Context, parent, child types.JID) error {
	_, err := cli.sendGroupIQ(ctx, iqSet, parent, waBinary.Node{
		Tag: "links",
		Content: []waBinary.Node{{
			Tag:   "link",
			Attrs: waBinary.Attrs{"link_type": string(types.GroupLinkChangeTypeSub)},
			Content: []waBinary.Node{{
				Tag:   "group",
				Attrs: waBinary.Attrs{"jid": child},
			}},
		}},
	})
	return err
}

// ReqLinkSubGroup is one group to link into a community with LinkSubGroups.
type ReqLinkSubGroup struct {
	JID types.JID
	// Hidden keeps the group out of the community's group list for people
	// who aren't in it. This can't be changed after linking.
	Hidden bool
}

// LinkSubGroups links several existing groups into a community in one
// request, reporting each group's outcome.
func (cli *Client) LinkSubGroups(ctx context.Context, parent types.JID, groups []ReqLinkSubGroup) ([]types.GroupLinkResult, error) {
	groupNodes := make([]waBinary.Node, len(groups))
	for i, group := range groups {
		groupNodes[i] = waBinary.Node{Tag: "group", Attrs: waBinary.Attrs{"jid": group.JID}}
		if group.Hidden {
			groupNodes[i].Content = []waBinary.Node{{Tag: "hidden_group"}}
		}
	}
	resp, err := cli.sendGroupIQ(ctx, iqSet, parent, waBinary.Node{
		Tag: "links",
		Content: []waBinary.Node{{
			Tag:     "link",
			Attrs:   waBinary.Attrs{"link_type": string(types.GroupLinkChangeTypeSub)},
			Content: groupNodes,
		}},
	})
	if err != nil {
		return nil, err
	}
	links, ok := resp.GetOptionalChildByTag("links", "link")
	if !ok {
		return nil, &ElementMissingError{Tag: "link", In: "response to link groups query"}
	}
	return parseGroupLinkResults(&links), nil
}

// UnlinkSubGroups removes several groups from a community in one request.
// With removeOrphanMembers, people who are only in the community through
// those groups are removed from the community too.
func (cli *Client) UnlinkSubGroups(ctx context.Context, parent types.JID, children []types.JID, removeOrphanMembers bool) ([]types.GroupLinkResult, error) {
	groupNodes := make([]waBinary.Node, len(children))
	for i, child := range children {
		attrs := waBinary.Attrs{"jid": child}
		if removeOrphanMembers {
			attrs["remove_orphaned_members"] = "true"
		}
		groupNodes[i] = waBinary.Node{Tag: "group", Attrs: attrs}
	}
	resp, err := cli.sendGroupIQ(ctx, iqSet, parent, waBinary.Node{
		Tag:     "unlink",
		Attrs:   waBinary.Attrs{"unlink_type": string(types.GroupLinkChangeTypeSub)},
		Content: groupNodes,
	})
	if err != nil {
		return nil, err
	}
	unlink, ok := resp.GetOptionalChildByTag("unlink")
	if !ok {
		return nil, &ElementMissingError{Tag: "unlink", In: "response to unlink groups query"}
	}
	return parseGroupLinkResults(&unlink), nil
}

func parseGroupLinkResults(node *waBinary.Node) []types.GroupLinkResult {
	var results []types.GroupLinkResult
	for _, child := range node.GetChildrenByTag("group") {
		ag := child.AttrGetter()
		results = append(results, types.GroupLinkResult{
			JID:   ag.JID("jid"),
			Error: ag.OptionalInt("error"),
		})
	}
	return results
}

// DeleteCommunity deactivates a community. Its groups are unlinked, not
// deleted.
func (cli *Client) DeleteCommunity(ctx context.Context, parent types.JID) error {
	_, err := cli.sendGroupIQ(ctx, iqSet, parent, waBinary.Node{Tag: "delete_parent"})
	return err
}

// ErrJoinRequiresApproval is returned by JoinLinkedGroup when the group asks
// admins to approve new members: a request to join was sent instead.
var ErrJoinRequiresApproval = errors.New("joining this group requires admin approval, a request was sent")

// JoinLinkedGroup joins a group in a community you're a member of, the way
// WhatsApp Web's "Join group" in a community does.
func (cli *Client) JoinLinkedGroup(ctx context.Context, parent, child types.JID) error {
	resp, err := cli.sendGroupIQ(ctx, iqSet, parent, waBinary.Node{
		Tag:   "join_linked_group",
		Attrs: waBinary.Attrs{"jid": child},
	})
	if err != nil {
		return err
	}
	if _, ok := resp.GetOptionalChildByTag("membership_approval_request"); ok {
		return ErrJoinRequiresApproval
	}
	return nil
}

// QueryLinkedGroup gets the info of a group in a community through the
// community, which works for groups you aren't in.
func (cli *Client) QueryLinkedGroup(ctx context.Context, parent, child types.JID) (*types.GroupInfo, error) {
	resp, err := cli.sendGroupIQ(ctx, iqGet, parent, waBinary.Node{
		Tag: "query_linked",
		Attrs: waBinary.Attrs{
			"type": string(types.GroupLinkChangeTypeSub),
			"jid":  child,
		},
	})
	if err != nil {
		return nil, err
	}
	groupNode, ok := resp.GetOptionalChildByTag("linked_group", "group")
	if !ok {
		return nil, &ElementMissingError{Tag: "group", In: "response to linked group query"}
	}
	return cli.parseGroupNode(&groupNode)
}

// RemoveCommunityParticipants removes people from a community and from every
// group in it.
func (cli *Client) RemoveCommunityParticipants(ctx context.Context, parent types.JID, participants []types.JID) ([]types.GroupParticipant, error) {
	content := make([]waBinary.Node, len(participants))
	for i, participant := range participants {
		content[i] = waBinary.Node{Tag: "participant", Attrs: waBinary.Attrs{"jid": participant}}
	}
	resp, err := cli.sendGroupIQ(ctx, iqSet, parent, waBinary.Node{
		Tag:     string(ParticipantChangeRemove),
		Attrs:   waBinary.Attrs{"linked_groups": "true"},
		Content: content,
	})
	if err != nil {
		return nil, err
	}
	removed, ok := resp.GetOptionalChildByTag(string(ParticipantChangeRemove))
	if !ok {
		return nil, &ElementMissingError{Tag: string(ParticipantChangeRemove), In: "response to community participant removal"}
	}
	children := removed.GetChildrenByTag("participant")
	results := make([]types.GroupParticipant, len(children))
	for i, child := range children {
		results[i] = parseParticipant(child.AttrGetter(), &child)
	}
	return results, nil
}

// LeaveGroup leaves the specified group on WhatsApp.
func (cli *Client) LeaveGroup(ctx context.Context, jid types.JID) error {
	_, err := cli.sendGroupIQ(ctx, iqSet, types.GroupServerJID, waBinary.Node{
		Tag: "leave",
		Content: []waBinary.Node{{
			Tag:   "group",
			Attrs: waBinary.Attrs{"id": jid},
		}},
	})
	return err
}

type ParticipantChange string

const (
	ParticipantChangeAdd     ParticipantChange = "add"
	ParticipantChangeRemove  ParticipantChange = "remove"
	ParticipantChangePromote ParticipantChange = "promote"
	ParticipantChangeDemote  ParticipantChange = "demote"
)

// UpdateGroupParticipants can be used to add, remove, promote and demote members in a WhatsApp group.
func (cli *Client) UpdateGroupParticipants(ctx context.Context, jid types.JID, participantChanges []types.JID, action ParticipantChange) ([]types.GroupParticipant, error) {
	content := make([]waBinary.Node, len(participantChanges))
	for i, participantJID := range participantChanges {
		content[i] = waBinary.Node{
			Tag:   "participant",
			Attrs: waBinary.Attrs{"jid": participantJID},
		}
		if participantJID.Server == types.HiddenUserServer && action == ParticipantChangeAdd {
			pn, err := cli.Store.LIDs.GetPNForLID(ctx, participantJID)
			if err != nil {
				return nil, fmt.Errorf("failed to get phone number for LID %s: %v", participantJID, err)
			} else if !pn.IsEmpty() {
				content[i].Attrs["phone_number"] = pn
			}
		}
		if action == ParticipantChangeAdd {
			token, err := cli.ensureTCToken(ctx, participantJID)
			if err != nil {
				return nil, fmt.Errorf("failed to get privacy token for participant %s: %v", participantJID, err)
			} else if len(token) > 0 {
				content[i].Content = []waBinary.Node{{
					Tag:     "privacy",
					Content: token,
				}}
			}
		}
	}
	resp, err := cli.sendGroupIQ(ctx, iqSet, jid, waBinary.Node{
		Tag:     string(action),
		Content: content,
	})
	if err != nil {
		return nil, err
	}
	requestAction, ok := resp.GetOptionalChildByTag(string(action))
	if !ok {
		return nil, &ElementMissingError{Tag: string(action), In: "response to group participants update"}
	}
	requestParticipants := requestAction.GetChildrenByTag("participant")
	participants := make([]types.GroupParticipant, len(requestParticipants))
	for i, child := range requestParticipants {
		participants[i] = parseParticipant(child.AttrGetter(), &child)
	}
	return participants, nil
}

// GetGroupRequestParticipants gets the list of participants that have requested to join the group.
func (cli *Client) GetGroupRequestParticipants(ctx context.Context, jid types.JID) ([]types.GroupParticipantRequest, error) {
	resp, err := cli.sendGroupIQ(ctx, iqGet, jid, waBinary.Node{
		Tag: "membership_approval_requests",
	})
	if err != nil {
		return nil, err
	}
	request, ok := resp.GetOptionalChildByTag("membership_approval_requests")
	if !ok {
		return nil, &ElementMissingError{Tag: "membership_approval_requests", In: "response to group request participants query"}
	}
	requestParticipants := request.GetChildrenByTag("membership_approval_request")
	participants := make([]types.GroupParticipantRequest, len(requestParticipants))
	for i, req := range requestParticipants {
		participants[i] = types.GroupParticipantRequest{
			JID:         req.AttrGetter().JID("jid"),
			RequestedAt: req.AttrGetter().UnixTime("request_time"),
		}
	}
	return participants, nil
}

type ParticipantRequestChange string

const (
	ParticipantChangeApprove ParticipantRequestChange = "approve"
	ParticipantChangeReject  ParticipantRequestChange = "reject"
)

// UpdateGroupRequestParticipants can be used to approve or reject requests to join the group.
func (cli *Client) UpdateGroupRequestParticipants(ctx context.Context, jid types.JID, participantChanges []types.JID, action ParticipantRequestChange) ([]types.GroupParticipant, error) {
	content := make([]waBinary.Node, len(participantChanges))
	for i, participantJID := range participantChanges {
		content[i] = waBinary.Node{
			Tag:   "participant",
			Attrs: waBinary.Attrs{"jid": participantJID},
		}
	}
	resp, err := cli.sendGroupIQ(ctx, iqSet, jid, waBinary.Node{
		Tag: "membership_requests_action",
		Content: []waBinary.Node{{
			Tag:     string(action),
			Content: content,
		}},
	})
	if err != nil {
		return nil, err
	}
	request, ok := resp.GetOptionalChildByTag("membership_requests_action")
	if !ok {
		return nil, &ElementMissingError{Tag: "membership_requests_action", In: "response to group request participants update"}
	}
	requestAction, ok := request.GetOptionalChildByTag(string(action))
	if !ok {
		return nil, &ElementMissingError{Tag: string(action), In: "response to group request participants update"}
	}
	requestParticipants := requestAction.GetChildrenByTag("participant")
	participants := make([]types.GroupParticipant, len(requestParticipants))
	for i, child := range requestParticipants {
		participants[i] = parseParticipant(child.AttrGetter(), &child)
	}
	return participants, nil
}

// SetGroupPhoto updates the group picture/icon of the given group on WhatsApp.
// The avatar should be a JPEG photo, other formats may be rejected with ErrInvalidImageFormat.
// The bytes can be nil to remove the photo. Returns the new picture ID.
func (cli *Client) SetGroupPhoto(ctx context.Context, jid types.JID, avatar []byte) (string, error) {
	var content any
	if avatar != nil {
		content = []waBinary.Node{{
			Tag:     "picture",
			Attrs:   waBinary.Attrs{"type": "image"},
			Content: avatar,
		}}
	}
	resp, err := cli.sendIQ(ctx, infoQuery{
		Namespace: "w:profile:picture",
		Type:      iqSet,
		To:        types.ServerJID,
		Target:    jid,
		Content:   content,
	})
	if errors.Is(err, ErrIQNotAcceptable) {
		return "", wrapIQError(ErrInvalidImageFormat, err)
	} else if err != nil {
		return "", err
	}
	if avatar == nil {
		return "remove", nil
	}
	pictureID, ok := resp.GetChildByTag("picture").Attrs["id"].(string)
	if !ok {
		return "", fmt.Errorf("didn't find picture ID in response")
	}
	return pictureID, nil
}

// SetGroupName updates the name (subject) of the given group on WhatsApp.
func (cli *Client) SetGroupName(ctx context.Context, jid types.JID, name string) error {
	_, err := cli.sendGroupIQ(ctx, iqSet, jid, waBinary.Node{
		Tag:     "subject",
		Content: []byte(name),
	})
	return err
}

// SetGroupTopic updates the topic (description) of the given group on WhatsApp.
//
// The previousID and newID fields are optional. If the previous ID is not specified, this will
// automatically fetch the current group info to find the previous topic ID. If the new ID is not
// specified, one will be generated with Client.GenerateMessageID().
func (cli *Client) SetGroupTopic(ctx context.Context, jid types.JID, previousID, newID, topic string) error {
	if previousID == "" {
		oldInfo, err := cli.GetGroupInfo(ctx, jid)
		if err != nil {
			return fmt.Errorf("failed to get old group info to update topic: %v", err)
		}
		previousID = oldInfo.TopicID
	}
	if newID == "" {
		newID = cli.GenerateMessageID()
	}
	attrs := waBinary.Attrs{
		"id": newID,
	}
	if previousID != "" {
		attrs["prev"] = previousID
	}
	content := []waBinary.Node{{
		Tag:     "body",
		Content: []byte(topic),
	}}
	if len(topic) == 0 {
		attrs["delete"] = "true"
		content = nil
	}
	_, err := cli.sendGroupIQ(ctx, iqSet, jid, waBinary.Node{
		Tag:     "description",
		Attrs:   attrs,
		Content: content,
	})
	return err
}

// SetGroupLocked changes whether the group is locked (i.e. whether only admins can modify group info).
func (cli *Client) SetGroupLocked(ctx context.Context, jid types.JID, locked bool) error {
	tag := "locked"
	if !locked {
		tag = "unlocked"
	}
	_, err := cli.sendGroupIQ(ctx, iqSet, jid, waBinary.Node{Tag: tag})
	return err
}

// SetGroupAnnounce changes whether the group is in announce mode (i.e. whether only admins can send messages).
func (cli *Client) SetGroupAnnounce(ctx context.Context, jid types.JID, announce bool) error {
	tag := "announcement"
	if !announce {
		tag = "not_announcement"
	}
	_, err := cli.sendGroupIQ(ctx, iqSet, jid, waBinary.Node{Tag: tag})
	return err
}

// GetGroupInviteLink requests the invite link to the group from the WhatsApp servers.
//
// If reset is true, then the old invite link will be revoked and a new one generated.
func (cli *Client) GetGroupInviteLink(ctx context.Context, jid types.JID, reset bool) (string, error) {
	iqType := iqGet
	if reset {
		iqType = iqSet
	}
	resp, err := cli.sendGroupIQ(ctx, iqType, jid, waBinary.Node{Tag: "invite"})
	if errors.Is(err, ErrIQNotAuthorized) {
		return "", wrapIQError(ErrGroupInviteLinkUnauthorized, err)
	} else if errors.Is(err, ErrIQNotFound) {
		return "", wrapIQError(ErrGroupNotFound, err)
	} else if errors.Is(err, ErrIQForbidden) {
		return "", wrapIQError(ErrNotInGroup, err)
	} else if err != nil {
		return "", err
	}
	code, ok := resp.GetChildByTag("invite").Attrs["code"].(string)
	if !ok {
		return "", fmt.Errorf("didn't find invite code in response")
	}
	return InviteLinkPrefix + code, nil
}

// GetGroupInfoFromInvite gets the group info from an invite message.
//
// Note that this is specifically for invite messages, not invite links. Use GetGroupInfoFromLink for resolving chat.whatsapp.com links.
func (cli *Client) GetGroupInfoFromInvite(ctx context.Context, jid, inviter types.JID, code string, expiration int64) (*types.GroupInfo, error) {
	resp, err := cli.sendGroupIQ(ctx, iqGet, jid, waBinary.Node{
		Tag: "query",
		Content: []waBinary.Node{{
			Tag: "add_request",
			Attrs: waBinary.Attrs{
				"code":       code,
				"expiration": expiration,
				"admin":      inviter,
			},
		}},
	})
	if err != nil {
		return nil, err
	}
	groupNode, ok := groupOrCommunityChild(resp)
	if !ok {
		return nil, &ElementMissingError{Tag: "group", In: "response to invite group info query"}
	}
	return cli.parseGroupNode(&groupNode)
}

// JoinGroupWithInvite joins a group using an invite message.
//
// Note that this is specifically for invite messages, not invite links. Use JoinGroupWithLink for joining with chat.whatsapp.com links.
//
// If the group asks admins to approve new members, a request to join is sent
// and ErrJoinRequiresApproval is returned.
func (cli *Client) JoinGroupWithInvite(ctx context.Context, jid, inviter types.JID, code string, expiration int64) error {
	if expiration > 0 && time.Unix(expiration, 0).Before(time.Now()) {
		return ErrInviteExpired
	}
	resp, err := cli.sendGroupIQ(ctx, iqSet, jid, waBinary.Node{
		Tag: "accept",
		Attrs: waBinary.Attrs{
			"code":       code,
			"expiration": expiration,
			"admin":      inviter,
		},
	})
	if err != nil {
		return err
	}
	if _, ok := resp.GetOptionalChildByTag("membership_approval_request"); ok {
		return ErrJoinRequiresApproval
	}
	return nil
}

// ErrInviteExpired is returned by JoinGroupWithInvite for an invite message
// whose expiry time has passed.
var ErrInviteExpired = errors.New("this group invite has expired")

// groupOrCommunityChild finds the group in a response, which names it
// <community> instead of <group> when it's a community.
func groupOrCommunityChild(node *waBinary.Node) (waBinary.Node, bool) {
	if group, ok := node.GetOptionalChildByTag("group"); ok {
		return group, true
	}
	return node.GetOptionalChildByTag("community")
}

// inviteCode pulls the code out of a group invite link in any of the forms
// WhatsApp uses (chat.whatsapp.com/CODE, chat.whatsapp.com/invite/CODE,
// ...?code=CODE), or returns the input when it's already a bare code.
func inviteCode(link string) string {
	link = strings.TrimSpace(link)
	if parsed, err := url.Parse(link); err == nil {
		if code := parsed.Query().Get("code"); code != "" {
			return code
		}
		if parsed.Host == "chat.whatsapp.com" {
			path := strings.Trim(parsed.Path, "/")
			return strings.TrimPrefix(path, "invite/")
		}
	}
	return strings.TrimSuffix(stripURLPrefix(link, InviteLinkPrefix), "/")
}

// GetGroupInfoFromLink resolves the given invite link and asks the WhatsApp servers for info about the group.
// This will not cause the user to join the group.
func (cli *Client) GetGroupInfoFromLink(ctx context.Context, code string) (*types.GroupInfo, error) {
	resp, err := cli.sendGroupIQ(ctx, iqGet, types.GroupServerJID, waBinary.Node{
		Tag: "invite",
		Attrs: waBinary.Attrs{
			"code": inviteCode(code),
		},
	})
	if errors.Is(err, ErrIQGone) {
		return nil, wrapIQError(ErrInviteLinkRevoked, err)
	} else if errors.Is(err, ErrIQNotAcceptable) {
		return nil, wrapIQError(ErrInviteLinkInvalid, err)
	} else if err != nil {
		return nil, err
	}
	groupNode, ok := groupOrCommunityChild(resp)
	if !ok {
		return nil, &ElementMissingError{Tag: "group", In: "response to group link info query"}
	}
	return cli.parseGroupNode(&groupNode)
}

// JoinGroupWithLink joins the group using the given invite link.
//
// If the group asks admins to approve new members, a request to join is sent
// and the group's JID is returned together with ErrJoinRequiresApproval.
func (cli *Client) JoinGroupWithLink(ctx context.Context, code string) (types.JID, error) {
	resp, err := cli.sendGroupIQ(ctx, iqSet, types.GroupServerJID, waBinary.Node{
		Tag: "invite",
		Attrs: waBinary.Attrs{
			"code": inviteCode(code),
		},
	})
	if errors.Is(err, ErrIQGone) {
		return types.EmptyJID, wrapIQError(ErrInviteLinkRevoked, err)
	} else if errors.Is(err, ErrIQNotAcceptable) {
		return types.EmptyJID, wrapIQError(ErrInviteLinkInvalid, err)
	} else if err != nil {
		return types.EmptyJID, err
	}
	membershipApprovalModeNode, ok := resp.GetOptionalChildByTag("membership_approval_request")
	if ok {
		// Not joined yet: a request went to the admins.
		return membershipApprovalModeNode.AttrGetter().JID("jid"), ErrJoinRequiresApproval
	}
	groupNode, ok := groupOrCommunityChild(resp)
	if !ok {
		return types.EmptyJID, &ElementMissingError{Tag: "group", In: "response to group link join query"}
	}
	return groupNode.AttrGetter().JID("jid"), nil
}

// GetJoinedGroups returns the list of groups the user is participating in.
func (cli *Client) GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error) {
	resp, err := cli.sendGroupIQ(ctx, iqGet, types.GroupServerJID, waBinary.Node{
		Tag: "participating",
		Content: []waBinary.Node{
			{Tag: "participants"},
			{Tag: "description"},
		},
	})
	if err != nil {
		return nil, err
	}
	groups, ok := resp.GetOptionalChildByTag("groups")
	if !ok {
		return nil, &ElementMissingError{Tag: "groups", In: "response to group list query"}
	}
	children := groups.GetChildren()
	infos := make([]*types.GroupInfo, 0, len(children))
	var allLIDPairs []store.LIDMapping
	var allRedactedPhones []store.RedactedPhoneEntry
	for _, child := range children {
		if child.Tag != "group" {
			cli.Log.Debugf("Unexpected child in group list response: %s", &child)
			continue
		}
		parsed, parseErr := cli.parseGroupNode(&child)
		if parseErr != nil {
			cli.Log.Warnf("Error parsing group %s: %v", parsed.JID, parseErr)
		}
		lidPairs, redactedPhones := cli.cacheGroupInfo(parsed, true)
		allLIDPairs = append(allLIDPairs, lidPairs...)
		allRedactedPhones = append(allRedactedPhones, redactedPhones...)
		infos = append(infos, parsed)
	}
	err = cli.Store.LIDs.PutManyLIDMappings(ctx, allLIDPairs)
	if err != nil {
		cli.Log.Warnf("Failed to store LID mappings from joined groups: %v", err)
	}
	err = cli.Store.Contacts.PutManyRedactedPhones(ctx, allRedactedPhones)
	if err != nil {
		cli.Log.Warnf("Failed to store redacted phones from joined groups: %v", err)
	}
	return infos, nil
}

// GetSubGroups gets the subgroups of the given community.
func (cli *Client) GetSubGroups(ctx context.Context, community types.JID) ([]*types.GroupLinkTarget, error) {
	res, err := cli.sendGroupIQ(ctx, iqGet, community, waBinary.Node{Tag: "sub_groups"})
	if err != nil {
		return nil, err
	}
	groups, ok := res.GetOptionalChildByTag("sub_groups")
	if !ok {
		return nil, &ElementMissingError{Tag: "sub_groups", In: "response to subgroups query"}
	}
	var parsedGroups []*types.GroupLinkTarget
	for _, child := range groups.GetChildren() {
		if child.Tag == "group" {
			parsedGroup, err := parseGroupLinkTargetNode(&child)
			if err != nil {
				return parsedGroups, fmt.Errorf("failed to parse group in subgroups list: %w", err)
			}
			parsedGroups = append(parsedGroups, &parsedGroup)
		}
	}
	return parsedGroups, nil
}

// GetLinkedGroupsParticipants gets all the participants in the groups of the given community.
func (cli *Client) GetLinkedGroupsParticipants(ctx context.Context, community types.JID) ([]types.JID, error) {
	res, err := cli.sendGroupIQ(ctx, iqGet, community, waBinary.Node{Tag: "linked_groups_participants"})
	if err != nil {
		return nil, err
	}
	participants, ok := res.GetOptionalChildByTag("linked_groups_participants")
	if !ok {
		return nil, &ElementMissingError{Tag: "linked_groups_participants", In: "response to community participants query"}
	}
	members, lidPairs, _ := parseParticipantList(&participants)
	if len(lidPairs) > 0 {
		err = cli.Store.LIDs.PutManyLIDMappings(ctx, lidPairs)
		if err != nil {
			cli.Log.Warnf("Failed to store LID mappings for community participants: %v", err)
		}
	}
	return members, nil
}

// GetGroupInfo requests basic info about a group chat from the WhatsApp servers.
func (cli *Client) GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
	return cli.getGroupInfo(ctx, jid, true)
}

func (cli *Client) cacheGroupInfo(groupInfo *types.GroupInfo, lock bool) ([]store.LIDMapping, []store.RedactedPhoneEntry) {
	participants := make([]types.JID, len(groupInfo.Participants))
	lidPairs := make([]store.LIDMapping, len(groupInfo.Participants))
	redactedPhones := make([]store.RedactedPhoneEntry, 0)
	for i, part := range groupInfo.Participants {
		participants[i] = part.JID
		if !part.PhoneNumber.IsEmpty() && !part.LID.IsEmpty() {
			lidPairs[i] = store.LIDMapping{
				LID: part.LID,
				PN:  part.PhoneNumber,
			}
		}
		if part.DisplayName != "" && !part.LID.IsEmpty() {
			redactedPhones = append(redactedPhones, store.RedactedPhoneEntry{
				JID:           part.LID,
				RedactedPhone: part.DisplayName,
			})
		}
	}
	if lock {
		cli.groupCacheLock.Lock()
		defer cli.groupCacheLock.Unlock()
	}
	cli.groupCache[groupInfo.JID] = &groupMetaCache{
		AddressingMode:             groupInfo.AddressingMode,
		CommunityAnnouncementGroup: groupInfo.IsAnnounce && groupInfo.IsDefaultSubGroup,
		Members:                    participants,
	}
	return lidPairs, redactedPhones
}

func (cli *Client) getGroupInfo(ctx context.Context, jid types.JID, lockParticipantCache bool) (*types.GroupInfo, error) {
	res, err := cli.sendGroupIQ(ctx, iqGet, jid, waBinary.Node{
		Tag:   "query",
		Attrs: waBinary.Attrs{"request": "interactive"},
	})
	if errors.Is(err, ErrIQNotFound) {
		return nil, wrapIQError(ErrGroupNotFound, err)
	} else if errors.Is(err, ErrIQForbidden) {
		return nil, wrapIQError(ErrNotInGroup, err)
	} else if err != nil {
		return nil, err
	}

	groupNode, ok := groupOrCommunityChild(res)
	if !ok {
		return nil, &ElementMissingError{Tag: "groups", In: "response to group info query"}
	}
	groupInfo, err := cli.parseGroupNode(&groupNode)
	if err != nil {
		return groupInfo, err
	}
	lidPairs, redactedPhones := cli.cacheGroupInfo(groupInfo, lockParticipantCache)
	err = cli.Store.LIDs.PutManyLIDMappings(ctx, lidPairs)
	if err != nil {
		cli.Log.Warnf("Failed to store LID mappings for members of %s: %v", jid, err)
	}
	err = cli.Store.Contacts.PutManyRedactedPhones(ctx, redactedPhones)
	if err != nil {
		cli.Log.Warnf("Failed to store redacted phones for members of %s: %v", jid, err)
	}
	return groupInfo, nil
}

func (cli *Client) getCachedGroupData(ctx context.Context, jid types.JID) (*groupMetaCache, error) {
	cli.groupCacheLock.Lock()
	defer cli.groupCacheLock.Unlock()
	if val, ok := cli.groupCache[jid]; ok {
		return val, nil
	}
	_, err := cli.getGroupInfo(ctx, jid, false)
	if err != nil {
		return nil, err
	}
	return cli.groupCache[jid], nil
}

func parseParticipant(childAG *waBinary.AttrUtility, child *waBinary.Node) types.GroupParticipant {
	pcpType := childAG.OptionalString("type")
	participant := types.GroupParticipant{
		IsAdmin:      pcpType == "admin" || pcpType == "superadmin",
		IsSuperAdmin: pcpType == "superadmin",
		JID:          childAG.JID("jid"),
		DisplayName:  childAG.OptionalString("display_name"),
	}
	if participant.JID.Server == types.HiddenUserServer {
		participant.LID = participant.JID
		participant.PhoneNumber = childAG.OptionalJIDOrEmpty("phone_number")
	} else if participant.JID.Server == types.DefaultUserServer {
		participant.PhoneNumber = participant.JID
		participant.LID = childAG.OptionalJIDOrEmpty("lid")
	}
	if errorCode := childAG.OptionalInt("error"); errorCode != 0 {
		participant.Error = errorCode
		addRequest, ok := child.GetOptionalChildByTag("add_request")
		if ok {
			addAG := addRequest.AttrGetter()
			participant.AddRequest = &types.GroupParticipantAddRequest{
				Code:       addAG.String("code"),
				Expiration: addAG.UnixTime("expiration"),
			}
		}
	}
	return participant
}

func (cli *Client) parseGroupNode(groupNode *waBinary.Node) (*types.GroupInfo, error) {
	var group types.GroupInfo
	ag := groupNode.AttrGetter()

	group.JID = types.NewJID(ag.String("id"), types.GroupServer)
	// A community comes as <community> in some responses, without a <parent>.
	group.IsParent = groupNode.Tag == "community"
	group.OwnerJID = ag.OptionalJIDOrEmpty("creator")
	group.OwnerPN = ag.OptionalJIDOrEmpty("creator_pn")

	group.Name = ag.OptionalString("subject")
	group.NameSetAt = ag.OptionalUnixTime("s_t")
	group.NameSetBy = ag.OptionalJIDOrEmpty("s_o")
	group.NameSetByPN = ag.OptionalJIDOrEmpty("s_o_pn")

	group.GroupCreated = ag.OptionalUnixTime("creation")
	group.CreatorCountryCode = ag.OptionalString("creator_country_code")

	group.AnnounceVersionID = ag.OptionalString("a_v_id")
	group.ParticipantVersionID = ag.OptionalString("p_v_id")
	group.ParticipantCount = ag.OptionalInt("size")
	group.AddressingMode = types.AddressingMode(ag.OptionalString("addressing_mode"))

	for _, child := range groupNode.GetChildren() {
		childAG := child.AttrGetter()
		switch child.Tag {
		case "participant":
			group.Participants = append(group.Participants, parseParticipant(childAG, &child))
		case "description":
			body, bodyOK := child.GetOptionalChildByTag("body")
			if bodyOK {
				topicBytes, _ := body.Content.([]byte)
				group.Topic = string(topicBytes)
				group.TopicID = childAG.String("id")
				group.TopicSetBy = childAG.OptionalJIDOrEmpty("participant")
				group.TopicSetByPN = childAG.OptionalJIDOrEmpty("participant_pn") // TODO confirm field name
				group.TopicSetAt = childAG.UnixTime("t")
			}
		case "announcement":
			group.IsAnnounce = true
		case "locked":
			group.IsLocked = true
		case "ephemeral":
			group.IsEphemeral = true
			group.DisappearingTimer = uint32(childAG.Uint64("expiration"))
		case "member_add_mode":
			modeBytes, _ := child.Content.([]byte)
			group.MemberAddMode = types.GroupMemberAddMode(modeBytes)
		case "linked_parent":
			group.LinkedParentJID = childAG.JID("jid")
		case "default_sub_group":
			group.IsDefaultSubGroup = true
		case "parent":
			group.IsParent = true
			group.DefaultMembershipApprovalMode = childAG.OptionalString("default_membership_approval_mode")
		case "allow_non_admin_sub_group_creation":
			group.AllowNonAdminSubGroupCreation = true
		case "general_chat":
			group.IsGeneralChat = true
		case "hidden_group":
			group.IsHiddenGroup = true
		case "incognito":
			group.IsIncognito = true
		case "membership_approval_mode":
			group.IsJoinApprovalRequired = membershipApprovalEnabled(&child)
		case "suspended":
			group.Suspended = true
		default:
			cli.Log.Debugf("Unknown element in group node %s: %s", group.JID.String(), &child)
		}
		if !childAG.OK() {
			cli.Log.Warnf("Possibly failed to parse %s element in group node: %+v", child.Tag, childAG.Errors)
		}
	}

	return &group, ag.Error()
}

func parseGroupLinkTargetNode(groupNode *waBinary.Node) (types.GroupLinkTarget, error) {
	ag := groupNode.AttrGetter()
	jidKey := ag.OptionalJIDOrEmpty("jid")
	if jidKey.IsEmpty() {
		jidKey = types.NewJID(ag.String("id"), types.GroupServer)
	}
	_, isGeneralChat := groupNode.GetOptionalChildByTag("general_chat")
	_, isHidden := groupNode.GetOptionalChildByTag("hidden_group")
	return types.GroupLinkTarget{
		JID: jidKey,
		GroupName: types.GroupName{
			Name:      ag.OptionalString("subject"),
			NameSetAt: ag.OptionalUnixTime("s_t"),
		},
		GroupIsDefaultSub: types.GroupIsDefaultSub{
			IsDefaultSubGroup: groupNode.GetChildByTag("default_sub_group").Tag == "default_sub_group",
		},
		GroupSubGroupProperties: types.GroupSubGroupProperties{
			IsGeneralChat: isGeneralChat,
			IsHiddenGroup: isHidden,
		},
		ParticipantCount: ag.OptionalInt("size"),
	}, ag.Error()
}

// membershipApprovalEnabled reads a <membership_approval_mode> element, which
// carries <group_join state="on|off"/> - it's present when approval is
// turned off too.
func membershipApprovalEnabled(node *waBinary.Node) bool {
	join, ok := node.GetOptionalChildByTag("group_join")
	if !ok {
		return true
	}
	return join.AttrGetter().OptionalString("state") != "off"
}

func parseParticipantList(node *waBinary.Node) (participants []types.JID, lidPairs []store.LIDMapping, redactedPhones []store.RedactedPhoneEntry) {
	children := node.GetChildren()
	participants = make([]types.JID, 0, len(children))
	for _, child := range children {
		jid, ok := child.Attrs["jid"].(types.JID)
		if child.Tag != "participant" || !ok {
			continue
		}
		participants = append(participants, jid)
		// Someone who hides their number comes with it masked
		// ("+91∙∙∙∙∙∙∙∙62"), the same as in full group info.
		if displayName, ok := child.Attrs["display_name"].(string); ok && displayName != "" && jid.Server == types.HiddenUserServer {
			redactedPhones = append(redactedPhones, store.RedactedPhoneEntry{JID: jid, RedactedPhone: displayName})
		}
		if jid.Server == types.HiddenUserServer {
			phoneNumber, ok := child.Attrs["phone_number"].(types.JID)
			if ok && !phoneNumber.IsEmpty() {
				lidPairs = append(lidPairs, store.LIDMapping{
					LID: jid,
					PN:  phoneNumber,
				})
			}
		} else if jid.Server == types.DefaultUserServer {
			lid, ok := child.Attrs["lid"].(types.JID)
			if ok && !lid.IsEmpty() {
				lidPairs = append(lidPairs, store.LIDMapping{
					LID: lid,
					PN:  jid,
				})
			}
		}
	}
	return
}

func (cli *Client) parseGroupCreate(parentNode, node *waBinary.Node) (*events.JoinedGroup, []store.LIDMapping, []store.RedactedPhoneEntry, error) {
	groupNode, ok := node.GetOptionalChildByTag("group")
	if !ok {
		return nil, nil, nil, fmt.Errorf("group create notification didn't contain group info")
	}
	var evt events.JoinedGroup
	pag := parentNode.AttrGetter()
	ag := node.AttrGetter()
	evt.Reason = ag.OptionalString("reason")
	evt.CreateKey = ag.OptionalString("key")
	evt.Type = ag.OptionalString("type")
	evt.Sender = pag.OptionalJID("participant")
	evt.SenderPN = pag.OptionalJID("participant_pn")
	evt.Notify = pag.OptionalString("notify")
	info, err := cli.parseGroupNode(&groupNode)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to parse group info in create notification: %w", err)
	}
	if info.AddressingMode == "" {
		info.AddressingMode = types.AddressingMode(pag.OptionalString("addressing_mode"))
	}
	evt.GroupInfo = *info
	lidPairs, redactedPhones := cli.cacheGroupInfo(info, true)
	return &evt, lidPairs, redactedPhones, nil
}

func (cli *Client) parseGroupChange(node *waBinary.Node) (*events.GroupInfo, []store.LIDMapping, []store.RedactedPhoneEntry, error) {
	var evt events.GroupInfo
	ag := node.AttrGetter()
	evt.JID = ag.JID("from")
	evt.Notify = ag.OptionalString("notify")
	evt.Sender = ag.OptionalJID("participant")
	evt.SenderPN = ag.OptionalJID("participant_pn")
	evt.Timestamp = ag.UnixTime("t")
	if !ag.OK() {
		return nil, nil, nil, fmt.Errorf("group change doesn't contain required attributes: %w", ag.Error())
	}

	var lidPairs []store.LIDMapping
	var redactedPhones []store.RedactedPhoneEntry
	// Whoever made the change is named by both addresses in a group that
	// hides numbers.
	if evt.Sender != nil && evt.SenderPN != nil && evt.Sender.Server == types.HiddenUserServer && evt.SenderPN.Server == types.DefaultUserServer {
		lidPairs = append(lidPairs, store.LIDMapping{LID: evt.Sender.ToNonAD(), PN: evt.SenderPN.ToNonAD()})
	}
	// One notification can carry several participant lists (say, an add and
	// a promote), so every list's mappings are kept, not just the last one's.
	participantList := func(child *waBinary.Node) []types.JID {
		jids, pairs, redacted := parseParticipantList(child)
		lidPairs = append(lidPairs, pairs...)
		redactedPhones = append(redactedPhones, redacted...)
		return jids
	}
	for _, child := range node.GetChildren() {
		cag := child.AttrGetter()
		if child.Tag == "add" || child.Tag == "remove" || child.Tag == "promote" || child.Tag == "demote" {
			evt.PrevParticipantVersionID = cag.OptionalString("prev_v_id")
			evt.ParticipantVersionID = cag.OptionalString("v_id")
		}
		switch child.Tag {
		case "add":
			evt.JoinReason = cag.OptionalString("reason")
			evt.Join = participantList(&child)
		case "remove":
			evt.Leave = participantList(&child)
		case "promote":
			evt.Promote = participantList(&child)
		case "demote":
			evt.Demote = participantList(&child)
		case "modify":
			evt.Modify = participantList(&child)
		case "linked_group_promote":
			evt.LinkedGroupPromote = participantList(&child)
		case "linked_group_demote":
			evt.LinkedGroupDemote = participantList(&child)
		case "allow_non_admin_sub_group_creation", "not_allow_non_admin_sub_group_creation":
			allowed := child.Tag == "allow_non_admin_sub_group_creation"
			evt.AllowNonAdminSubGroupCreation = &allowed
		case "member_add_mode":
			modeBytes, _ := child.Content.([]byte)
			mode := types.GroupMemberAddMode(modeBytes)
			evt.MemberAddMode = &mode
		case "locked":
			evt.Locked = &types.GroupLocked{IsLocked: true}
		case "unlocked":
			evt.Locked = &types.GroupLocked{IsLocked: false}
		case "delete":
			evt.Delete = &types.GroupDelete{Deleted: true, DeleteReason: cag.OptionalString("reason")}
		case "subject":
			evt.Name = &types.GroupName{
				Name:        cag.String("subject"),
				NameSetAt:   cag.UnixTime("s_t"),
				NameSetBy:   cag.OptionalJIDOrEmpty("s_o"),
				NameSetByPN: cag.OptionalJIDOrEmpty("s_o_pn"),
			}
		case "description":
			var topicStr string
			_, isDelete := child.GetOptionalChildByTag("delete")
			if !isDelete {
				topicChild := child.GetChildByTag("body")
				topicBytes, ok := topicChild.Content.([]byte)
				if !ok {
					return nil, nil, nil, fmt.Errorf("group change description has unexpected body: %s", &topicChild)
				}
				topicStr = string(topicBytes)
			}
			var setBy types.JID
			if evt.Sender != nil {
				setBy = *evt.Sender
			}
			evt.Topic = &types.GroupTopic{
				Topic:        topicStr,
				TopicID:      cag.String("id"),
				TopicSetAt:   evt.Timestamp,
				TopicSetBy:   setBy,
				TopicDeleted: isDelete,
			}
		case "announcement":
			evt.Announce = &types.GroupAnnounce{
				IsAnnounce:        true,
				AnnounceVersionID: cag.String("v_id"),
			}
		case "not_announcement":
			evt.Announce = &types.GroupAnnounce{
				IsAnnounce:        false,
				AnnounceVersionID: cag.String("v_id"),
			}
		case "invite":
			link := InviteLinkPrefix + cag.String("code")
			evt.NewInviteLink = &link
		case "ephemeral":
			timer := uint32(cag.Uint64("expiration"))
			evt.Ephemeral = &types.GroupEphemeral{
				IsEphemeral:       true,
				DisappearingTimer: timer,
			}
		case "not_ephemeral":
			evt.Ephemeral = &types.GroupEphemeral{IsEphemeral: false}
		case "link":
			evt.Link = &types.GroupLinkChange{
				Type: types.GroupLinkChangeType(cag.String("link_type")),
			}
			groupNode, ok := child.GetOptionalChildByTag("group")
			if !ok {
				return nil, nil, nil, &ElementMissingError{Tag: "group", In: "group link"}
			}
			var err error
			evt.Link.Group, err = parseGroupLinkTargetNode(&groupNode)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("failed to parse group link node in group change: %w", err)
			}
		case "unlink":
			evt.Unlink = &types.GroupLinkChange{
				Type:         types.GroupLinkChangeType(cag.String("unlink_type")),
				UnlinkReason: types.GroupUnlinkReason(cag.String("unlink_reason")),
			}
			groupNode, ok := child.GetOptionalChildByTag("group")
			if !ok {
				return nil, nil, nil, &ElementMissingError{Tag: "group", In: "group unlink"}
			}
			var err error
			evt.Unlink.Group, err = parseGroupLinkTargetNode(&groupNode)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("failed to parse group unlink node in group change: %w", err)
			}
		case "membership_approval_mode":
			evt.MembershipApprovalMode = &types.GroupMembershipApprovalMode{
				IsJoinApprovalRequired: membershipApprovalEnabled(&child),
			}
		case "suspended":
			evt.Suspended = true
		case "unsuspended":
			evt.Unsuspended = true
		default:
			evt.UnknownChanges = append(evt.UnknownChanges, &child)
		}
		if !cag.OK() {
			return nil, nil, nil, fmt.Errorf("group change %s element doesn't contain required attributes: %w", child.Tag, cag.Error())
		}
	}
	return &evt, lidPairs, redactedPhones, nil
}

func (cli *Client) updateGroupParticipantCache(evt *events.GroupInfo) {
	// TODO can the addressing mode change here?
	if len(evt.Join) == 0 && len(evt.Leave) == 0 {
		return
	}
	cli.groupCacheLock.Lock()
	defer cli.groupCacheLock.Unlock()
	cached, ok := cli.groupCache[evt.JID]
	if !ok {
		return
	}
Outer:
	for _, jid := range evt.Join {
		for _, existingJID := range cached.Members {
			if jid == existingJID {
				continue Outer
			}
		}
		cached.Members = append(cached.Members, jid)
	}
	for _, jid := range evt.Leave {
		for i, existingJID := range cached.Members {
			if existingJID == jid {
				cached.Members[i] = cached.Members[len(cached.Members)-1]
				cached.Members = cached.Members[:len(cached.Members)-1]
				break
			}
		}
	}
}

func (cli *Client) parseGroupNotification(node *waBinary.Node) (any, []store.LIDMapping, []store.RedactedPhoneEntry, error) {
	children := node.GetChildren()
	if len(children) == 1 && children[0].Tag == "create" {
		return cli.parseGroupCreate(node, &children[0])
	} else if len(children) > 0 && children[0].Tag == "groups_dirty" {
		// Sent by the server about itself (from is s.whatsapp.net), so it
		// must not be read as a change to a group.
		return cli.handleGroupsDirty(&children[0]), nil, nil, nil
	} else {
		groupChange, lidPairs, redactedPhones, err := cli.parseGroupChange(node)
		if err != nil {
			return nil, nil, nil, err
		}
		cli.updateGroupParticipantCache(groupChange)
		return groupChange, lidPairs, redactedPhones, nil
	}
}

// handleGroupsDirty drops the cached info of the groups the server says is
// stale, so the next send to each fetches its member list again rather than
// encrypting for devices that are no longer in it.
func (cli *Client) handleGroupsDirty(node *waBinary.Node) *events.GroupsDirty {
	var evt events.GroupsDirty
	for _, child := range node.GetChildrenByTag("group") {
		if jid, ok := child.Attrs["jid"].(types.JID); ok {
			evt.Groups = append(evt.Groups, jid)
		}
	}
	cli.groupCacheLock.Lock()
	for _, jid := range evt.Groups {
		delete(cli.groupCache, jid)
	}
	cli.groupCacheLock.Unlock()
	return &evt
}

// rotateGroupSenderKey throws away our sender key for a group, so the next
// message there starts a new one. WhatsApp Web does this when someone leaves
// or is removed, or changes number, so they can't read what we send after.
// (Every group message already carries the key to all current members, so
// nothing else has to happen.)
func (cli *Client) rotateGroupSenderKey(ctx context.Context, group types.JID) {
	empty := groupRecord.NewSenderKey(store.SignalProtobufSerializer.SenderKeyRecord, store.SignalProtobufSerializer.SenderKeyState)
	for _, own := range []types.JID{cli.getOwnLID(), cli.getOwnID()} {
		if own.IsEmpty() {
			continue
		}
		name := protocol.NewSenderKeyName(group.String(), own.SignalAddress())
		if err := cli.Store.StoreSenderKey(ctx, name, empty); err != nil {
			cli.Log.Warnf("Failed to rotate our sender key for %s: %v", group, err)
		}
	}
}

// afterGroupChange keeps what we hold for a group in step with a change to
// it: a member list that no longer matches is dropped, and our sender key is
// replaced when someone who could read it is gone.
func (cli *Client) afterGroupChange(ctx context.Context, evt *events.GroupInfo) {
	if len(evt.Modify) > 0 {
		cli.groupCacheLock.Lock()
		delete(cli.groupCache, evt.JID)
		cli.groupCacheLock.Unlock()
	}
	othersLeft := false
	for _, jid := range evt.Leave {
		if !cli.isOwnJID(jid) {
			othersLeft = true
			break
		}
	}
	if othersLeft || len(evt.Modify) > 0 {
		cli.rotateGroupSenderKey(ctx, evt.JID)
	}
}

func (cli *Client) isOwnJID(jid types.JID) bool {
	jid = jid.ToNonAD()
	return jid == cli.getOwnID().ToNonAD() || jid == cli.getOwnLID().ToNonAD()
}

// SetGroupJoinApprovalMode sets the group join approval mode to 'on' or 'off'.
func (cli *Client) SetGroupJoinApprovalMode(ctx context.Context, jid types.JID, mode bool) error {
	modeStr := "off"
	if mode {
		modeStr = "on"
	}

	content := waBinary.Node{
		Tag: "membership_approval_mode",
		Content: []waBinary.Node{
			{
				Tag:   "group_join",
				Attrs: waBinary.Attrs{"state": modeStr},
			},
		},
	}

	_, err := cli.sendGroupIQ(ctx, iqSet, jid, content)
	return err
}

// SetGroupMemberAddMode sets the group member add mode to 'admin_add' or 'all_member_add'.
func (cli *Client) SetGroupMemberAddMode(ctx context.Context, jid types.JID, mode types.GroupMemberAddMode) error {
	if mode != types.GroupMemberAddModeAdmin && mode != types.GroupMemberAddModeAllMember {
		return errors.New("invalid mode, must be 'admin_add' or 'all_member_add'")
	}

	content := waBinary.Node{
		Tag:     "member_add_mode",
		Content: []byte(mode),
	}

	_, err := cli.sendGroupIQ(ctx, iqSet, jid, content)
	return err
}

// Deprecated: duplicate of SetGroupTopic
func (cli *Client) SetGroupDescription(ctx context.Context, jid types.JID, description string) error {
	return cli.SetGroupTopic(ctx, jid, "", "", description)
}
