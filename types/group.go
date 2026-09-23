// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package types

import (
	"time"
)

type GroupMemberAddMode string

const (
	GroupMemberAddModeAdmin     GroupMemberAddMode = "admin_add"
	GroupMemberAddModeAllMember GroupMemberAddMode = "all_member_add"
)

// GroupInfo contains basic information about a group chat on WhatsApp.
type GroupInfo struct {
	JID      JID
	OwnerJID JID
	OwnerPN  JID

	GroupName
	GroupTopic
	GroupLocked
	GroupAnnounce
	GroupEphemeral
	GroupIncognito

	GroupParent
	GroupLinkedParent
	GroupIsDefaultSub
	GroupSubGroupProperties
	GroupMembershipApprovalMode

	AddressingMode     AddressingMode
	GroupCreated       time.Time
	CreatorCountryCode string

	ParticipantVersionID string
	Participants         []GroupParticipant
	ParticipantCount     int

	MemberAddMode GroupMemberAddMode

	// Suspended indicates whether the group is currently paused/suspended.
	Suspended bool
}

type GroupMembershipApprovalMode struct {
	IsJoinApprovalRequired bool
}

type GroupParent struct {
	IsParent                      bool
	DefaultMembershipApprovalMode string // request_required
	// AllowNonAdminSubGroupCreation is set on a community whose members may
	// add groups to it without being admins.
	AllowNonAdminSubGroupCreation bool
}

// GroupSubGroupProperties describes a group linked into a community.
type GroupSubGroupProperties struct {
	// IsGeneralChat marks the community's general chat, which every member is
	// added to automatically.
	IsGeneralChat bool
	// IsHiddenGroup marks a group that isn't listed in the community for
	// people who aren't in it.
	IsHiddenGroup bool
}

type GroupLinkedParent struct {
	LinkedParentJID JID
}

type GroupIsDefaultSub struct {
	IsDefaultSubGroup bool
}

// GroupName contains the name of a group along with metadata of who set it and when.
type GroupName struct {
	Name        string
	NameSetAt   time.Time
	NameSetBy   JID
	NameSetByPN JID
}

// GroupTopic contains the topic (description) of a group along with metadata of who set it and when.
type GroupTopic struct {
	Topic        string
	TopicID      string
	TopicSetAt   time.Time
	TopicSetBy   JID
	TopicSetByPN JID
	TopicDeleted bool
}

// GroupLocked specifies whether the group info can only be edited by admins.
type GroupLocked struct {
	IsLocked bool
}

// GroupAnnounce specifies whether only admins can send messages in the group.
type GroupAnnounce struct {
	IsAnnounce        bool
	AnnounceVersionID string
}

type GroupIncognito struct {
	IsIncognito bool
}

// GroupParticipant contains info about a participant of a WhatsApp group chat.
type GroupParticipant struct {
	// The primary JID that should be used to send messages to this participant.
	// Always equals either the LID or phone number.
	JID         JID
	PhoneNumber JID
	LID         JID

	IsAdmin      bool
	IsSuperAdmin bool

	// This is only present for anonymous users in announcement groups, it's an obfuscated phone number
	DisplayName string

	// When creating groups, adding some participants may fail.
	// In such cases, the error code will be here.
	Error      int
	AddRequest *GroupParticipantAddRequest
}

type GroupParticipantAddRequest struct {
	Code       string
	Expiration time.Time
}

// GroupEphemeral contains the group's disappearing messages settings.
type GroupEphemeral struct {
	IsEphemeral       bool
	DisappearingTimer uint32
}

type GroupDelete struct {
	Deleted      bool
	DeleteReason string
}

type GroupLinkChangeType string

const (
	GroupLinkChangeTypeParent  GroupLinkChangeType = "parent_group"
	GroupLinkChangeTypeSub     GroupLinkChangeType = "sub_group"
	GroupLinkChangeTypeSibling GroupLinkChangeType = "sibling_group"
)

type GroupUnlinkReason string

const (
	GroupUnlinkReasonDefault GroupUnlinkReason = "unlink_group"
	GroupUnlinkReasonDelete  GroupUnlinkReason = "delete_parent"
)

type GroupLinkTarget struct {
	JID JID
	GroupName
	GroupIsDefaultSub
	GroupSubGroupProperties
	// ParticipantCount is the group's size, when the server includes it.
	ParticipantCount int
}

// GroupLinkResult is one group's outcome in a batch link or unlink request.
type GroupLinkResult struct {
	JID JID
	// Error is the server's error code for this group, or 0 if it succeeded.
	Error int
}

type GroupLinkChange struct {
	Type         GroupLinkChangeType
	UnlinkReason GroupUnlinkReason
	Group        GroupLinkTarget
}

type GroupParticipantRequest struct {
	JID         JID
	RequestedAt time.Time
}
