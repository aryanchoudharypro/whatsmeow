package whatsmeow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Two peer data operation requests to the primary phone around history sync.
// Neither is sent by WA Web (its WAWebNonMessagePeerDataRequest logging throws
// "history sync chunk retry is not supported in web" and "full history sync on
// demand is not supported in web"), so the shapes come from the
// PeerDataOperationRequestMessage protobuf the phone shares with the native
// clients:
//
//   - HISTORY_SYNC_CHUNK_RETRY (historySyncChunkRetryRequest): syncType,
//     chunkOrder, chunkNotificationID (the ID of the protocol message that
//     carried the HistorySyncNotification) and regenerateChunk. The phone
//     answers with a new notification for that chunk, or with a
//     historySyncChunkRetryResponse whose code says why it can't
//     (GENERATION_ERROR, CHUNK_CONSUMED, TIMEOUT, SESSION_EXHAUSTED,
//     CHUNK_EXHAUSTED, DUPLICATED_REQUEST) and canRecover.
//   - FULL_HISTORY_SYNC_ON_DEMAND (fullHistorySyncOnDemandRequest):
//     requestMetadata {requestID, businessProduct, opaqueClientData,
//     connection}, a HistorySyncConfig like the one in the registration
//     payload, and fullHistorySyncOnDemandConfig {historyFromTimestamp,
//     historyDurationDays}. The answer is a fullHistorySyncOnDemandRequestResponse
//     echoing the metadata with a code; the history itself comes as ordinary
//     history sync notifications carrying fullHistorySyncOnDemandRequestMetadata.
//     The codes (ERROR_REQUEST_ON_NON_SMB_PRIMARY, hosted device and
//     multi-provider errors) show it was built for WhatsApp Business
//     coexistence, so a personal phone may well refuse it.
//
// Both go out with the default peer push priority ("high"), as whatsapp-rust's
// peer_message_options_from_message does for every PDO type but the few it
// marks high_force.

// historySyncChunkRetryLimit is how many times one chunk is asked for again.
const historySyncChunkRetryLimit = 3

// BuildHistorySyncChunkRetryRequest builds a request asking the primary phone
// to send one history sync chunk again. notificationID is the message ID of
// the protocol message that carried the chunk's notification. With regenerate
// set, the phone is asked to rebuild and upload the chunk again (the blob was
// gone or broken) rather than only resend its notification.
//
// The built message can be sent using Client.SendPeerMessage.
func BuildHistorySyncChunkRetryRequest(syncType waE2E.HistorySyncType, chunkOrder uint32, notificationID types.MessageID, regenerate bool) *waE2E.Message {
	return &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_MESSAGE.Enum(),
			PeerDataOperationRequestMessage: &waE2E.PeerDataOperationRequestMessage{
				PeerDataOperationRequestType: waE2E.PeerDataOperationRequestType_HISTORY_SYNC_CHUNK_RETRY.Enum(),
				HistorySyncChunkRetryRequest: &waE2E.PeerDataOperationRequestMessage_HistorySyncChunkRetryRequest{
					SyncType:            syncType.Enum(),
					ChunkOrder:          proto.Uint32(chunkOrder),
					ChunkNotificationID: proto.String(string(notificationID)),
					RegenerateChunk:     proto.Bool(regenerate),
				},
			},
		},
	}
}

// BuildFullHistorySyncOnDemandRequest builds a request asking the primary
// phone for its full history again, as at linking, without relinking.
// historyFrom and durationDays bound it (zero values leave them out); config
// is the history sync config to ask with, nil for the one in
// store.DeviceProps. requestID comes back in the phone's answer
// (events.FullHistorySyncOnDemandResult).
//
// The built message can be sent using Client.SendPeerMessage.
func BuildFullHistorySyncOnDemandRequest(requestID string, historyFrom time.Time, durationDays uint32, config *waCompanionReg.DeviceProps_HistorySyncConfig) *waE2E.Message {
	if config == nil {
		config = proto.Clone(store.DeviceProps.GetHistorySyncConfig()).(*waCompanionReg.DeviceProps_HistorySyncConfig)
	}
	onDemand := &waE2E.FullHistorySyncOnDemandConfig{}
	if !historyFrom.IsZero() {
		onDemand.HistoryFromTimestamp = proto.Uint64(uint64(historyFrom.UnixMilli()))
	}
	if durationDays > 0 {
		onDemand.HistoryDurationDays = proto.Uint32(durationDays)
	}
	return &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_MESSAGE.Enum(),
			PeerDataOperationRequestMessage: &waE2E.PeerDataOperationRequestMessage{
				PeerDataOperationRequestType: waE2E.PeerDataOperationRequestType_FULL_HISTORY_SYNC_ON_DEMAND.Enum(),
				FullHistorySyncOnDemandRequest: &waE2E.PeerDataOperationRequestMessage_FullHistorySyncOnDemandRequest{
					RequestMetadata: &waE2E.FullHistorySyncOnDemandRequestMetadata{
						RequestID: proto.String(requestID),
					},
					HistorySyncConfig:             config,
					FullHistorySyncOnDemandConfig: onDemand,
				},
			},
		},
	}
}

// historySyncChunkGone reports whether a failed chunk download means the blob
// itself is gone or broken, so only a rebuilt upload can help.
func historySyncChunkGone(err error) bool {
	return errors.Is(err, ErrMediaDownloadFailedWith403) ||
		errors.Is(err, ErrMediaDownloadFailedWith404) ||
		errors.Is(err, ErrMediaDownloadFailedWith410) ||
		errors.Is(err, ErrInvalidMediaHMAC) ||
		errors.Is(err, ErrInvalidMediaEncSHA256) ||
		errors.Is(err, ErrInvalidMediaSHA256)
}

func historySyncChunkKey(notif *waE2E.HistorySyncNotification) string {
	return fmt.Sprintf("%s_%d", notif.GetSyncType(), notif.GetChunkOrder())
}

// retryFailedHistorySyncChunk asks the phone for a chunk whose download
// failed, when Client.RetryFailedHistorySyncChunks is on.
func (cli *Client) retryFailedHistorySyncChunk(ctx context.Context, dl *historySyncDownload) {
	notif := dl.notif
	switch {
	case !cli.RetryFailedHistorySyncChunks, dl.msgID == "":
		return
	case notif.GetSyncType() == waE2E.HistorySyncType_ON_DEMAND,
		notif.GetSyncType() == waE2E.HistorySyncType_INITIAL_STATUS_V3:
		// An on-demand answer is simply asked for again by the caller, and
		// statuses expire anyway.
		return
	}
	key := historySyncChunkKey(notif)
	cli.historyChunkRetriesLock.Lock()
	if cli.historyChunkRetries == nil {
		cli.historyChunkRetries = make(map[string]int)
	}
	attempt := cli.historyChunkRetries[key] + 1
	if attempt > historySyncChunkRetryLimit {
		cli.historyChunkRetriesLock.Unlock()
		cli.Log.Warnf("History sync chunk %s still failing after %d retries, giving up on it", key, historySyncChunkRetryLimit)
		return
	}
	cli.historyChunkRetries[key] = attempt
	cli.historyChunkRetriesLock.Unlock()

	regenerate := historySyncChunkGone(dl.err)
	msg := BuildHistorySyncChunkRetryRequest(notif.GetSyncType(), notif.GetChunkOrder(), dl.msgID, regenerate)
	resp, err := cli.SendPeerMessage(ctx, msg)
	if err != nil {
		cli.Log.Warnf("Failed to ask the phone to resend history sync chunk %s: %v", key, err)
		return
	}
	cli.Log.Infof("Asked the phone to resend history sync chunk %s (notification %s, regenerate %v, attempt %d, request %s)",
		key, dl.msgID, regenerate, attempt, resp.ID)
}

// handleHistorySyncChunkRetryResponse dispatches the phone's answers to chunk
// retry requests. A chunk it can resend just arrives as a new notification;
// these answers are mostly the reasons it can't.
func (cli *Client) handleHistorySyncChunkRetryResponse(resp *waE2E.PeerDataOperationRequestResponseMessage) {
	for _, result := range resp.GetPeerDataOperationResult() {
		r := result.GetHistorySyncChunkRetryResponse()
		if r == nil {
			continue
		}
		cli.Log.Infof("Phone answered history sync chunk retry for %s_%d (request %s): code %s, can recover %v",
			r.GetSyncType(), r.GetChunkOrder(), r.GetRequestID(), r.GetResponseCode(), r.GetCanRecover())
		cli.dispatchEvent(&events.HistorySyncChunkRetryResult{
			RequestID:  resp.GetStanzaID(),
			SyncType:   r.GetSyncType(),
			ChunkOrder: r.GetChunkOrder(),
			Code:       r.GetResponseCode(),
			HasCode:    r.ResponseCode != nil,
			CanRecover: r.GetCanRecover(),
		})
	}
}

func (cli *Client) handleFullHistorySyncOnDemandResponse(resp *waE2E.PeerDataOperationRequestResponseMessage) {
	for _, result := range resp.GetPeerDataOperationResult() {
		r := result.GetFullHistorySyncOnDemandRequestResponse()
		if r == nil {
			continue
		}
		cli.Log.Infof("Phone answered full history sync on demand %s: %s",
			r.GetRequestMetadata().GetRequestID(), r.GetResponseCode())
		cli.dispatchEvent(&events.FullHistorySyncOnDemandResult{
			RequestID: r.GetRequestMetadata().GetRequestID(),
			Code:      r.GetResponseCode(),
		})
	}
}
