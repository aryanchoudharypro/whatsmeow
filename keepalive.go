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

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

var (
	// KeepAliveResponseDeadline specifies the duration to wait for a response to websocket keepalive pings.
	KeepAliveResponseDeadline = 10 * time.Second
	// KeepAliveIntervalMin specifies the minimum interval for websocket keepalive pings.
	KeepAliveIntervalMin = 20 * time.Second
	// KeepAliveIntervalMax specifies the maximum interval for websocket keepalive pings.
	KeepAliveIntervalMax = 30 * time.Second

	// KeepAliveMaxFailTime specifies the maximum time to wait before forcing a reconnect if keepalives fail repeatedly.
	KeepAliveMaxFailTime = 3 * time.Minute
)

func (cli *Client) keepAliveLoop(ctx, connCtx context.Context) {
	lastSuccess := time.Now()
	var errorCount int
	for {
		interval := rand.Int64N(KeepAliveIntervalMax.Milliseconds()-KeepAliveIntervalMin.Milliseconds()) + KeepAliveIntervalMin.Milliseconds()
		select {
		case <-time.After(time.Duration(interval) * time.Millisecond):
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
					cli.Disconnect()
					cli.resetExpectedDisconnect()
					go cli.autoReconnect(ctx)
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
	respCh, err := cli.sendIQAsync(writeCtx, infoQuery{
		Namespace: "w:p",
		Type:      "get",
		To:        types.ServerJID,
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
