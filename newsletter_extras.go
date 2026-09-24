// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// More of the persisted queries WhatsApp Web uses for channels.
const (
	mutationPinNewsletterMessages   = "27165709459706559" // WAWebMexNewsletterPinMessagesJobMutation
	mutationUnpinNewsletterMessages = "28007176042216937" // WAWebMexNewsletterUnpinMessagesJobMutation
	queryNewsletterReactionSenders  = "29575462448733991" // WAWebMexFetchNewsletterMessageReactionSenderListJobQuery
	queryNewsletterPollVoters       = "9407762219322536"  // WAWebMexFetchNewsletterPollVotersJobQuery
	queryNewsletterPendingInvites   = "9783111038412085"  // WAWebMexFetchNewsletterPendingInvitesJobQuery
	querySimilarNewsletters         = "26217043484590756" // WAWebMexFetchSimilarNewslettersJobQuery
)

// NewsletterSendPollVote votes in a channel poll. Channel polls are counted
// by the server, so the vote is the SHA-256 of each chosen option's name
// rather than an encrypted poll update. An empty optionNames takes the vote
// back.
func (cli *Client) NewsletterSendPollVote(ctx context.Context, jid types.JID, serverID types.MessageServerID, optionNames []string) error {
	votes := make([]waBinary.Node, len(optionNames))
	for i, name := range optionNames {
		hash := sha256.Sum256([]byte(name))
		votes[i] = waBinary.Node{Tag: "vote", Content: hash[:]}
	}
	return cli.sendNode(ctx, waBinary.Node{
		Tag: "message",
		Attrs: waBinary.Attrs{
			"to":        jid,
			"id":        cli.GenerateMessageID(),
			"type":      "poll",
			"server_id": serverID,
		},
		Content: []waBinary.Node{
			{Tag: "meta", Attrs: waBinary.Attrs{"polltype": "vote"}},
			{Tag: "votes", Content: votes},
		},
	})
}

type respNewsletterPins struct {
	ID     string `json:"id"`
	Thread struct {
		Pinned []types.NewsletterPinnedMessage `json:"pinned_messages"`
	} `json:"thread_metadata"`
}

// PinNewsletterMessages pins posts in a channel we own or run, by server
// ID. It returns what's pinned afterwards.
func (cli *Client) PinNewsletterMessages(ctx context.Context, jid types.JID, serverIDs []types.MessageServerID) ([]types.NewsletterPinnedMessage, error) {
	return cli.changeNewsletterPins(ctx, mutationPinNewsletterMessages, "xwa2_newsletter_pin_messages", jid, serverIDs)
}

// UnpinNewsletterMessages unpins posts in a channel we own or run.
func (cli *Client) UnpinNewsletterMessages(ctx context.Context, jid types.JID, serverIDs []types.MessageServerID) ([]types.NewsletterPinnedMessage, error) {
	return cli.changeNewsletterPins(ctx, mutationUnpinNewsletterMessages, "xwa2_newsletter_unpin_messages", jid, serverIDs)
}

func (cli *Client) changeNewsletterPins(ctx context.Context, queryID, field string, jid types.JID, serverIDs []types.MessageServerID) ([]types.NewsletterPinnedMessage, error) {
	ids := make([]string, len(serverIDs))
	for i, id := range serverIDs {
		ids[i] = strconv.Itoa(int(id))
	}
	data, err := cli.sendMexIQ(ctx, queryID, map[string]any{
		"newsletter_id": jid.String(),
		"input":         map[string]any{"message_ids": ids},
	})
	if err != nil {
		return nil, err
	}
	var resp map[string]*respNewsletterPins
	if err = json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse pinned posts: %w", err)
	} else if resp[field] == nil {
		return nil, fmt.Errorf("server didn't change the pinned posts")
	}
	return resp[field].Thread.Pinned, nil
}

// NewsletterReactionSenders is who reacted to a channel post with one
// emoji.
type NewsletterReactionSenders struct {
	Code    string
	Senders []types.JID
}

// GetNewsletterReactionSenders lists who reacted to a post in a channel we
// own or run, by emoji.
func (cli *Client) GetNewsletterReactionSenders(ctx context.Context, jid types.JID, serverID types.MessageServerID) ([]NewsletterReactionSenders, error) {
	data, err := cli.sendMexIQ(ctx, queryNewsletterReactionSenders, map[string]any{
		"input": map[string]any{"id": jid.String(), "server_id": strconv.Itoa(int(serverID))},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		List *struct {
			Reactions []struct {
				Code    string `json:"reaction_code"`
				Senders struct {
					Edges []struct {
						Node struct {
							ID string `json:"id"`
						} `json:"node"`
					} `json:"edges"`
				} `json:"sender_list"`
			} `json:"reactions"`
		} `json:"xwa2_newsletters_reaction_sender_list"`
	}
	if err = json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse reactions: %w", err)
	} else if resp.List == nil {
		return nil, fmt.Errorf("server didn't list the reactions")
	}
	out := make([]NewsletterReactionSenders, 0, len(resp.List.Reactions))
	for _, reaction := range resp.List.Reactions {
		entry := NewsletterReactionSenders{Code: reaction.Code}
		for _, edge := range reaction.Senders.Edges {
			if sender, err := types.ParseJID(edge.Node.ID); err == nil {
				entry.Senders = append(entry.Senders, sender)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// NewsletterPollVoter is someone who voted for a channel poll option.
type NewsletterPollVoter struct {
	JID      types.JID
	VoteTime int64
}

// GetNewsletterPollVoters lists who picked one option of a poll in a
// channel we own or run. optionName is the option as the poll lists it.
func (cli *Client) GetNewsletterPollVoters(ctx context.Context, jid types.JID, serverID types.MessageServerID, optionName string, limit int) ([]NewsletterPollVoter, error) {
	hash := sha256.Sum256([]byte(optionName))
	data, err := cli.sendMexIQ(ctx, queryNewsletterPollVoters, map[string]any{
		"input": map[string]any{
			"newsletter_id": jid.String(),
			"server_id":     strconv.Itoa(int(serverID)),
			"vote_hash":     base64.StdEncoding.EncodeToString(hash[:]),
			"limit":         limit,
		},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		List *struct {
			Votes []struct {
				Voters struct {
					Edges []struct {
						ActionTime json.RawMessage `json:"action_time"`
						Node       struct {
							ID string `json:"id"`
						} `json:"node"`
					} `json:"edges"`
				} `json:"voter_list"`
			} `json:"votes"`
		} `json:"voter_list"`
	}
	if err = json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse poll voters: %w", err)
	} else if resp.List == nil {
		return nil, fmt.Errorf("server didn't list the poll voters")
	}
	var out []NewsletterPollVoter
	for _, vote := range resp.List.Votes {
		for _, edge := range vote.Voters.Edges {
			if voter, err := types.ParseJID(edge.Node.ID); err == nil {
				out = append(out, NewsletterPollVoter{JID: voter, VoteTime: int64(looseInt(edge.ActionTime))})
			}
		}
	}
	return out, nil
}

// NewsletterPendingInvite is an admin invite that hasn't been accepted yet.
type NewsletterPendingInvite struct {
	JID   types.JID
	Phone types.JID
}

// GetNewsletterPendingAdminInvites lists the admin invites of a channel we
// own that haven't been accepted yet.
func (cli *Client) GetNewsletterPendingAdminInvites(ctx context.Context, jid types.JID) ([]NewsletterPendingInvite, error) {
	data, err := cli.sendMexIQ(ctx, queryNewsletterPendingInvites, map[string]any{"newsletter_id": jid.String()})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Admin *struct {
			Pending []struct {
				User struct {
					ID string `json:"id"`
					PN string `json:"pn"`
				} `json:"user"`
			} `json:"pending_admin_invites"`
		} `json:"xwa2_newsletter_admin"`
	}
	if err = json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse pending invites: %w", err)
	} else if resp.Admin == nil {
		return nil, fmt.Errorf("server didn't list the pending invites")
	}
	out := make([]NewsletterPendingInvite, 0, len(resp.Admin.Pending))
	for _, invite := range resp.Admin.Pending {
		user, err := types.ParseJID(invite.User.ID)
		if err != nil {
			continue
		}
		entry := NewsletterPendingInvite{JID: user}
		if invite.User.PN != "" {
			entry.Phone, _ = types.ParseJID(invite.User.PN)
		}
		out = append(out, entry)
	}
	return out, nil
}

// GetSimilarNewsletters lists channels like the given one, as the official
// apps suggest on a channel's info.
func (cli *Client) GetSimilarNewsletters(ctx context.Context, jid types.JID, limit int) ([]types.NewsletterDirectoryEntry, error) {
	data, err := cli.sendMexIQ(ctx, querySimilarNewsletters, map[string]any{
		"fetch_status_metadata": false,
		"input":                 map[string]any{"newsletter_id": jid.String(), "limit": limit},
	})
	if err != nil {
		return nil, err
	}
	page, err := parseNewsletterDirectoryPage(data, "xwa2_newsletters_similar")
	if err != nil {
		return nil, err
	}
	return page.Channels, nil
}
