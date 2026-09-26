package whatsmeow

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"go.mau.fi/whatsmeow/proto/waAICommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// inlineBotCapabilities are the reply formats WA Web tells a bot it can show
// (WAWebGenerateBotMetadata.generateBotCapabilityMetadata, without the ones
// behind unified-response gates).
var inlineBotCapabilities = []waAICommon.BotCapabilityMetadata_BotCapabilityType{
	waAICommon.BotCapabilityMetadata_RICH_RESPONSE_STRUCTURED_RESPONSE,
	waAICommon.BotCapabilityMetadata_RICH_RESPONSE_HEADING,
	waAICommon.BotCapabilityMetadata_RICH_RESPONSE_SUB_HEADING,
	waAICommon.BotCapabilityMetadata_RICH_RESPONSE_TABLE,
	waAICommon.BotCapabilityMetadata_RICH_RESPONSE_INLINE_REELS,
	waAICommon.BotCapabilityMetadata_SESSION_TRANSPARENCY_SYSTEM_MESSAGE,
}

// prepareInlineBotCopy turns the chat's message into the bot's copy, as WA
// Web's updateBotInvokeMsgProtoCopyForCapi does: a quote of anyone but a bot
// is dropped, and each "@<number>" mention becomes "@<push name>"
// (WAWebBotReplaceMentionWidsWithPushnames), so the bot reads names.
func (cli *Client) prepareInlineBotCopy(ctx context.Context, msg *waE2E.Message) {
	ext := msg.GetExtendedTextMessage()
	if ext == nil {
		return
	}
	if ci := ext.GetContextInfo(); ci != nil {
		if participant, err := types.ParseJID(ci.GetParticipant()); ci.QuotedMessage != nil && (err != nil || !participant.IsBot()) {
			ci.QuotedMessage = nil
			ci.StanzaID = nil
			ci.RemoteJID = nil
			ci.Participant = nil
		}
		replacements := map[string]string{}
		for _, raw := range ci.GetMentionedJID() {
			jid, err := types.ParseJID(raw)
			if err != nil || jid.User == "" {
				continue
			}
			if name := cli.inlineBotPushName(ctx, jid); name != "" {
				replacements["@"+jid.User] = "@" + name
			}
		}
		ext.Text = proto.String(replaceLongestFirst(ext.GetText(), replacements))
	}
}

func (cli *Client) inlineBotPushName(ctx context.Context, jid types.JID) string {
	candidates := []types.JID{jid}
	if jid.Server == types.HiddenUserServer && cli.Store.LIDs != nil {
		if pn, err := cli.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
			candidates = append(candidates, pn)
		}
	}
	for _, c := range candidates {
		if contact, err := cli.Store.Contacts.GetContact(ctx, c); err == nil && contact.Found {
			if contact.PushName != "" {
				return contact.PushName
			}
			if contact.BusinessName != "" {
				return contact.BusinessName
			}
		}
	}
	if jid.User == types.NewMetaAIJID.User && jid.Server == types.NewMetaAIJID.Server {
		return "Meta AI"
	}
	return ""
}

func replaceLongestFirst(text string, replacements map[string]string) string {
	keys := make([]string, 0, len(replacements))
	for k := range replacements {
		keys = append(keys, k)
	}
	// WA Web sorts by length, longest first, so "@123" can't eat "@12345".
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, k := range keys {
		text = strings.ReplaceAll(text, k, replacements[k])
	}
	return text
}
