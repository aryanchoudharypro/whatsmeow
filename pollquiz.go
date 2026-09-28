package whatsmeow

import (
	"encoding/hex"
	"fmt"

	"go.mau.fi/util/random"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// BuildQuizPollCreation builds a quiz: a single-choice poll with one right
// answer, optionNames[correctIndex], which the official apps reveal once
// someone votes. The built message can be sent normally using
// Client.SendMessage.
//
// Ported from whatsapp-rust's Polls::create_quiz (WA Web's
// GeneratePollCreationMessageProto).
func (cli *Client) BuildQuizPollCreation(name string, optionNames []string, correctIndex int) (*waE2E.Message, error) {
	switch {
	case len(optionNames) < 2:
		return nil, fmt.Errorf("a quiz needs at least 2 options")
	case len(optionNames) > 12:
		return nil, fmt.Errorf("a quiz can have at most 12 options")
	case correctIndex < 0 || correctIndex >= len(optionNames):
		return nil, fmt.Errorf("correct answer %d is out of range (quiz has %d options)", correctIndex, len(optionNames))
	}
	seen := make(map[string]bool, len(optionNames))
	options := make([]*waE2E.PollCreationMessage_Option, len(optionNames))
	for i, option := range optionNames {
		// Votes are option hashes, so two equal names couldn't be told apart.
		if seen[option] {
			return nil, fmt.Errorf("duplicate option name: %s", option)
		}
		seen[option] = true
		options[i] = &waE2E.PollCreationMessage_Option{OptionName: proto.String(option)}
	}
	correct := optionNames[correctIndex]
	return &waE2E.Message{
		// Single-choice polls go out as V3, as WA Web sends them.
		PollCreationMessageV3: &waE2E.PollCreationMessage{
			Name:                   proto.String(name),
			Options:                options,
			SelectableOptionsCount: proto.Uint32(1),
			PollContentType:        waE2E.PollContentType_TEXT.Enum(),
			PollType:               waE2E.PollType_QUIZ.Enum(),
			CorrectAnswer: &waE2E.PollCreationMessage_Option{
				OptionName: proto.String(correct),
				// The proto field is a string: lowercase hex of SHA-256(name).
				OptionHash: proto.String(hex.EncodeToString(HashPollOptions([]string{correct})[0])),
			},
		},
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: random.Bytes(32)},
	}, nil
}
