// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"errors"
	"fmt"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// StatusLikeEmoji is the reaction WA Web sends when you like a status
// (WAWebStatusLikeEmojis.STATUS_LIKE_EMOJI, a green heart).
const StatusLikeEmoji = "\U0001F49A"

// SendStatusReaction reacts to someone's status update, the way WA Web's
// status like button does (WAWebSendStatusReactionAction.sendStatusReaction).
// Pass StatusLikeEmoji to like the status and an empty reaction to take the
// like back.
//
// Unlike a status update, the reaction isn't sent with the status sender key:
// it's encrypted one by one for the poster's devices and our own other
// devices and sent to status@broadcast with a <participants> list
// (WAWebEncryptAndSendStatusMsg.encryptAndSendStatusDirectMsg), so only the
// poster sees it.
func (cli *Client) SendStatusReaction(ctx context.Context, poster types.JID, statusID types.MessageID, reaction string) (resp SendResponse, err error) {
	if cli == nil {
		err = ErrClientIsNil
		return
	}
	if poster.IsEmpty() || statusID == "" {
		err = errors.New("status reaction needs the poster and the status ID")
		return
	}
	if poster.Server != types.DefaultUserServer && poster.Server != types.HiddenUserServer {
		err = fmt.Errorf("status poster must be a user JID, got %s", poster)
		return
	}
	ownPN := cli.getOwnID().ToNonAD()
	if ownPN.IsEmpty() {
		err = ErrNotLoggedIn
		return
	}

	// The key points at the status as it was received: in status@broadcast,
	// from the poster (WAWebReferentialMsgKey.getReferentialMsgKey).
	message := cli.BuildReaction(types.StatusBroadcastJID, poster.ToNonAD(), statusID, reaction)

	// WA Web fans out to the poster and itself by LID
	// (WAWebSendStatusReactionAction: getFanOutList of [poster, me]).
	posterUser, ownUser := cli.statusReactionRecipients(ctx, poster.ToNonAD())

	resp.ID = cli.GenerateMessageID()
	resp.Sender = ownUser
	resp.Chat = types.StatusBroadcastJID

	start := time.Now()
	cli.messageSendLock.Lock()
	resp.DebugTimings.Queue = time.Since(start)
	defer cli.messageSendLock.Unlock()

	err = cli.addRecentMessage(ctx, types.StatusBroadcastJID, resp.ID, message, nil)
	if err != nil {
		return
	}

	start = time.Now()
	plaintext, dsmPlaintext, err := marshalMessage(types.StatusBroadcastJID, message)
	resp.DebugTimings.Marshal = time.Since(start)
	if err != nil {
		return
	}
	node, _, err := cli.prepareMessageNode(
		ctx, types.StatusBroadcastJID, resp.ID, message, []types.JID{posterUser, ownUser},
		plaintext, dsmPlaintext, &resp.DebugTimings, nodeExtraParams{metaNode: statusAddOnMetaNode()},
	)
	if err != nil {
		return
	}
	if !finishStatusDirectNode(node) {
		err = fmt.Errorf("failed to encrypt status reaction %s for any device of %s", resp.ID, posterUser)
		return
	}

	respChan := cli.waitResponse(resp.ID)
	start = time.Now()
	data, err := cli.sendNodeAndGetData(ctx, *node)
	resp.DebugTimings.Send = time.Since(start)
	if err != nil {
		cli.cancelResponse(resp.ID, respChan)
		err = fmt.Errorf("failed to send status reaction node: %w", err)
		return
	}

	start = time.Now()
	var respNode *waBinary.Node
	select {
	case respNode = <-respChan:
	case <-time.After(defaultRequestTimeout):
		cli.cancelResponse(resp.ID, respChan)
		err = ErrMessageTimedOut
		return
	case <-ctx.Done():
		cli.cancelResponse(resp.ID, respChan)
		err = ctx.Err()
		return
	}
	resp.DebugTimings.Resp = time.Since(start)
	if isDisconnectNode(respNode) {
		start = time.Now()
		respNode, err = cli.retryFrame(ctx, "status reaction send", resp.ID, data, respNode, 0)
		resp.DebugTimings.Retry = time.Since(start)
		if err != nil {
			return
		}
	}
	ag := respNode.AttrGetter()
	resp.Timestamp = ag.UnixTime("t")
	if errorCode := ag.Int("error"); errorCode != 0 {
		err = fmt.Errorf("%w %d", ErrServerReturnedError, errorCode)
	}
	return
}

// statusReactionRecipients picks who a status reaction is encrypted for: the
// poster and our own account, both by LID when the poster's LID is known.
func (cli *Client) statusReactionRecipients(ctx context.Context, poster types.JID) (posterUser, ownUser types.JID) {
	posterUser = poster
	if poster.Server == types.DefaultUserServer {
		lid, err := cli.Store.LIDs.GetLIDForPN(ctx, poster)
		if err != nil {
			cli.Log.Warnf("Failed to get LID for status poster %s: %v", poster, err)
		} else if !lid.IsEmpty() {
			posterUser = lid
		}
	}
	ownUser = cli.getOwnID().ToNonAD()
	if ownLID := cli.getOwnLID().ToNonAD(); posterUser.Server == types.HiddenUserServer && !ownLID.IsEmpty() {
		ownUser = ownLID
	}
	return
}

// statusAddOnMetaNode is the <meta> a reaction to a status carries
// (WAWebSendMsgMetaNode.getStatusContentType: an add-on in status@broadcast).
func statusAddOnMetaNode() *waBinary.Node {
	return &waBinary.Node{Tag: "meta", Attrs: waBinary.Attrs{"content_type": "add_on"}}
}

// finishStatusDirectNode shapes a pairwise-encrypted status@broadcast message
// like WA Web's GROUP_DIRECT fanout (WAWebSendMsgCreateFanoutStanza): an
// empty sender key <enc> goes right after <participants>. It reports false
// if the message wasn't encrypted for any device.
func finishStatusDirectNode(node *waBinary.Node) bool {
	children := node.GetChildren()
	if len(children) == 0 || children[0].Tag != "participants" || len(children[0].GetChildren()) == 0 {
		return false
	}
	skMsg := waBinary.Node{Tag: "enc", Attrs: waBinary.Attrs{"v": "2", "type": "skmsg"}}
	content := make([]waBinary.Node, 0, len(children)+1)
	content = append(content, children[0], skMsg)
	content = append(content, children[1:]...)
	node.Content = content
	return true
}
