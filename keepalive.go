// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

var (
	// KeepAliveResponseDeadline specifies the duration to wait for a response to websocket keepalive pings.
	KeepAliveResponseDeadline = 10 * time.Second
	// KeepAliveIntervalMin specifies the minimum interval for websocket keepalive pings.
	// WhatsApp Web pings every healthCheckInterval * (1 + random()), with an
	// interval of 15 seconds, which is anywhere from 15 to 30 seconds.
	KeepAliveIntervalMin = 15 * time.Second
	// KeepAliveIntervalMax specifies the maximum interval for websocket keepalive pings.
	KeepAliveIntervalMax = 30 * time.Second

	// KeepAliveMaxFailTime specifies the maximum time to wait before forcing a reconnect if keepalives fail repeatedly.
	KeepAliveMaxFailTime = 3 * time.Minute
	// DeadSocketTime is how long a request (a keepalive ping or any other
	// iq) may go without a single frame coming back before the socket is
	// treated as dead and reconnected, as WhatsApp Web's deadSocketTime.
	DeadSocketTime = 20 * time.Second
)

func (cli *Client) keepAliveLoop(ctx, connCtx context.Context) {
	lastSuccess := time.Now()
	var errorCount int
	for {
		interval := rand.Int64N(KeepAliveIntervalMax.Milliseconds()-KeepAliveIntervalMin.Milliseconds()) + KeepAliveIntervalMin.Milliseconds()
		select {
		case <-time.After(time.Duration(interval) * time.Millisecond):
			// WhatsApp Web only pings an idle socket: while a request is
			// still waiting for its answer, the dead socket timer is
			// already watching the connection.
			if cli.hasPendingResponses() {
				continue
			}
			isSuccess, shouldContinue, wedged := cli.sendKeepAlive(connCtx)
			if !shouldContinue {
				return
			} else if wedged && cli.EnableAutoReconnect {
				// The ping couldn't even be written: the socket is open but
				// no longer drains, so nothing will ever arrive on it.
				// Waiting out KeepAliveMaxFailTime would only prolong that.
				cli.Log.Warnf("Forcing reconnect: keepalive ping could not be written")
				cli.Disconnect()
				cli.resetExpectedDisconnect()
				go cli.autoReconnect(ctx)
				return
			} else if !isSuccess {
				errorCount++
				go cli.dispatchEvent(&events.KeepAliveTimeout{
					ErrorCount:  errorCount,
					LastSuccess: lastSuccess,
				})
				if cli.EnableAutoReconnect && time.Since(lastSuccess) > KeepAliveMaxFailTime {
					cli.Log.Debugf("Forcing reconnect due to keepalive failure")
					cli.ResetConnection()
				}
			} else {
				if errorCount > 0 {
					errorCount = 0
					go cli.dispatchEvent(&events.KeepAliveRestored{})
				}
				lastSuccess = time.Now()
			}
		case <-connCtx.Done():
			return
		}
	}
}

// sendKeepAlive pings the server. wedged reports that the ping could not be
// written within KeepAliveResponseDeadline, which means the socket is stuck
// rather than the server being slow.
func (cli *Client) sendKeepAlive(ctx context.Context) (isSuccess, shouldContinue, wedged bool) {
	// The deadline starts before the write, not after it: a write into a
	// socket that stopped draining never returns, and would park this loop
	// forever with the connection still looking up.
	deadline := time.Now().Add(KeepAliveResponseDeadline)
	writeCtx, cancelWrite := context.WithDeadline(ctx, deadline)
	id := cli.generateRequestID()
	cli.lastKeepAliveID.Store(&id)
	respCh, err := cli.sendIQAsync(writeCtx, infoQuery{
		Namespace: "w:p",
		Type:      "get",
		To:        types.ServerJID,
		ID:        id,
	})
	cancelWrite()
	if ctx.Err() != nil {
		return false, false, false
	} else if errors.Is(err, context.DeadlineExceeded) {
		cli.Log.Warnf("Keepalive ping write timed out")
		return false, true, true
	} else if err != nil {
		cli.Log.Warnf("Failed to send keepalive: %v", err)
		return false, true, false
	}
	select {
	case <-respCh:
		// All good
		return true, true, false
	case <-time.After(time.Until(deadline)):
		cli.Log.Warnf("Keepalive timed out")
		return false, true, false
	case <-ctx.Done():
		return false, false, false
	}
}

func (cli *Client) hasPendingResponses() bool {
	cli.responseWaitersLock.Lock()
	defer cli.responseWaitersLock.Unlock()
	return len(cli.responseWaiters) > 0
}

// deadSocketLoop drops the connection once a request has gone DeadSocketTime
// without any frame arriving, like WhatsApp Web's dead socket timer: every
// request arms it, every incoming frame disarms it. The socket is closed
// without a close handshake (nothing would answer one) and as a remote
// disconnect, so the usual Disconnected event and auto-reconnect follow.
func (cli *Client) deadSocketLoop(connCtx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-connCtx.Done():
			return
		case <-ticker.C:
		}
		since := cli.deadSocketSince.Load()
		if since == 0 || time.Since(time.Unix(0, since)) < DeadSocketTime {
			continue
		}
		cli.Log.Warnf("Socket dead: nothing received for %s after a request, reconnecting", DeadSocketTime)
		cli.socketLock.Lock()
		sock := cli.socket
		cli.socketLock.Unlock()
		if sock != nil && connCtx.Err() == nil {
			sock.Abort()
		}
		return
	}
}

func (cli *Client) isKeepAliveNode(node *waBinary.Node) bool {
	if node.Tag != "iq" || node.Content != nil {
		return false
	}
	id := cli.lastKeepAliveID.Load()
	return id != nil && node.Attrs["id"] == *id
}
