// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"go.mau.fi/whatsmeow/types"
)

// The persisted queries WhatsApp Web uses to create and run a channel.
const (
	mutationCreateNewsletterV2    = "25149874324715067" // WAWebMexCreateNewsletterJobMutation
	mutationUpdateNewsletterV2    = "24250201037901610" // WAWebMexUpdateNewsletterJobMutation
	mutationDeleteNewsletter      = "30062808666639665" // WAWebMexDeleteNewsletterJobMutation
	mutationChangeNewsletterOwner = "9546742745432473"  // WAWebMexChangeNewsletterOwnerJobMutation
	mutationDemoteNewsletterAdmin = "9880997548630971"  // WAWebMexDemoteNewsletterAdminJobMutation
	queryNewsletterFollowers      = "27472091235714801" // WAWebMexFetchNewsletterFollowersJobQuery
)

// NewsletterCreationNoticeID is the channel-creation notice WhatsApp shows
// before someone makes their first channel. Accept it with
// AcceptTOSNotice(NewsletterCreationNoticeID, "5") first.
const NewsletterCreationNoticeID = "20601218"

// createNewsletterV2 creates a channel with the query WhatsApp Web uses now.
func (cli *Client) createNewsletterV2(ctx context.Context, params CreateNewsletterParams) (*types.NewsletterMetadata, error) {
	input := map[string]any{"name": params.Name}
	if params.Description != "" {
		input["description"] = params.Description
	}
	if len(params.Picture) > 0 {
		input["picture"] = base64.StdEncoding.EncodeToString(params.Picture)
	}
	data, err := cli.sendMexIQ(ctx, mutationCreateNewsletterV2, map[string]any{"input": input})
	if err != nil {
		return nil, err
	}
	var resp respCreateNewsletter
	if err = json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse created channel: %w", err)
	} else if resp.Newsletter == nil {
		return nil, fmt.Errorf("server didn't create the channel")
	}
	return resp.Newsletter, nil
}

// NewsletterUpdate is a change to a channel's details. Nil fields are left
// as they are.
type NewsletterUpdate struct {
	Name        *string
	Description *string
	// Picture is a JPEG to use as the channel picture. An empty, non-nil
	// slice removes the picture.
	Picture *[]byte
}

// UpdateNewsletter changes a channel's name, description or picture. Only
// the channel's owner and admins can.
func (cli *Client) UpdateNewsletter(ctx context.Context, jid types.JID, update NewsletterUpdate) error {
	updates := map[string]any{}
	if update.Name != nil {
		updates["name"] = *update.Name
	}
	if update.Description != nil {
		updates["description"] = *update.Description
	}
	if update.Picture != nil {
		updates["picture"] = base64.StdEncoding.EncodeToString(*update.Picture)
	}
	if len(updates) == 0 {
		return nil
	}
	return cli.newsletterAdminMutation(ctx, mutationUpdateNewsletterV2, "xwa2_newsletter_update", map[string]any{
		"newsletter_id": jid.String(),
		"updates":       updates,
	})
}

// DeleteNewsletter deletes a channel for everyone. Only its owner can.
func (cli *Client) DeleteNewsletter(ctx context.Context, jid types.JID) error {
	return cli.newsletterAdminMutation(ctx, mutationDeleteNewsletter, "xwa2_newsletter_delete_v2", map[string]any{
		"newsletter_id": jid.String(),
	})
}

// ChangeNewsletterOwner hands a channel over to one of its admins. Only the
// owner can; they become an admin.
func (cli *Client) ChangeNewsletterOwner(ctx context.Context, jid, user types.JID) error {
	lid, err := cli.newsletterAdminTarget(ctx, user)
	if err != nil {
		return err
	}
	return cli.newsletterAdminMutation(ctx, mutationChangeNewsletterOwner, "xwa2_newsletter_change_owner", map[string]any{
		"newsletter_id": jid.String(),
		"user_id":       lid.String(),
	})
}

// DemoteNewsletterAdmin makes an admin of a channel a plain follower again.
// Only the owner can.
func (cli *Client) DemoteNewsletterAdmin(ctx context.Context, jid, user types.JID) error {
	lid, err := cli.newsletterAdminTarget(ctx, user)
	if err != nil {
		return err
	}
	return cli.newsletterAdminMutation(ctx, mutationDemoteNewsletterAdmin, "xwa2_newsletter_admin_demote", map[string]any{
		"newsletter_id": jid.String(),
		"user_id":       lid.String(),
	})
}

// newsletterAdminTarget is the LID the admin mutations address a person by;
// like WA Web, a phone number JID with no known LID is refused.
func (cli *Client) newsletterAdminTarget(ctx context.Context, user types.JID) (types.JID, error) {
	if user.Server == types.HiddenUserServer {
		return user.ToNonAD(), nil
	}
	lid, err := cli.Store.LIDs.GetLIDForPN(ctx, user.ToNonAD())
	if err != nil || lid.IsEmpty() {
		return types.EmptyJID, fmt.Errorf("no known LID for %s", user)
	}
	return lid, nil
}

// newsletterAdminMutation runs a channel mutation whose only answer is its
// result field; null there means the server didn't do it.
func (cli *Client) newsletterAdminMutation(ctx context.Context, mutation, resultField string, variables map[string]any) error {
	data, err := cli.sendMexIQ(ctx, mutation, variables)
	if err != nil {
		return err
	}
	var result map[string]json.RawMessage
	if err = json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("failed to parse channel response: %w", err)
	}
	if raw, ok := result[resultField]; !ok || string(raw) == "null" {
		return fmt.Errorf("server didn't confirm the change (%s missing)", resultField)
	}
	return nil
}

// NewsletterFollower is someone following a channel, as its owner and admins
// see them.
type NewsletterFollower struct {
	// JID is the follower's LID on accounts that have one.
	JID types.JID
	// PhoneJID is empty when the follower's privacy settings hide it.
	PhoneJID    types.JID
	DisplayName string
	Username    string
	Role        types.NewsletterRole
	FollowTime  int64
}

// GetNewsletterFollowers lists up to count of a channel's followers,
// admins included. Only the owner and admins can. There are no further
// pages: WA Web asks for one list.
func (cli *Client) GetNewsletterFollowers(ctx context.Context, jid types.JID, count int) ([]NewsletterFollower, error) {
	data, err := cli.sendMexIQ(ctx, queryNewsletterFollowers, map[string]any{
		"input": map[string]any{
			"newsletter_id": jid.String(),
			"count":         count,
		},
	})
	if err != nil {
		return nil, err
	}
	return parseNewsletterFollowers(data)
}

func parseNewsletterFollowers(data json.RawMessage) ([]NewsletterFollower, error) {
	var resp struct {
		Followers *struct {
			Followers struct {
				Edges []struct {
					Role       string          `json:"role"`
					FollowTime json.RawMessage `json:"follow_time"`
					Node       struct {
						ID           string `json:"id"`
						PN           string `json:"pn"`
						DisplayName  string `json:"display_name"`
						UsernameInfo struct {
							Username string `json:"username"`
						} `json:"username_info"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"followers"`
		} `json:"xwa2_newsletter_followers"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse channel followers: %w", err)
	}
	if resp.Followers == nil {
		return nil, fmt.Errorf("server didn't return the channel's followers")
	}
	followers := make([]NewsletterFollower, 0, len(resp.Followers.Followers.Edges))
	for _, edge := range resp.Followers.Followers.Edges {
		// WA Web drops an entry with no ID: there's nobody to address.
		jid, err := types.ParseJID(edge.Node.ID)
		if edge.Node.ID == "" || err != nil {
			continue
		}
		follower := NewsletterFollower{
			JID:         jid,
			DisplayName: edge.Node.DisplayName,
			Username:    edge.Node.UsernameInfo.Username,
			FollowTime:  int64(looseInt(edge.FollowTime)),
		}
		_ = follower.Role.UnmarshalText([]byte(edge.Role))
		if edge.Node.PN != "" {
			follower.PhoneJID, _ = types.ParseJID(edge.Node.PN)
		}
		followers = append(followers, follower)
	}
	return followers, nil
}
