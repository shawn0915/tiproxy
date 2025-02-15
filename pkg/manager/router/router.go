// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"time"

	glist "github.com/bahlo/generic-list-go"
	"github.com/pingcap/TiProxy/lib/util/errors"
)

var (
	ErrNoInstanceToSelect = errors.New("no instances to route")
)

// ConnEventReceiver receives connection events.
type ConnEventReceiver interface {
	OnRedirectSucceed(from, to string, conn RedirectableConn) error
	OnRedirectFail(from, to string, conn RedirectableConn) error
	OnConnClosed(addr string, conn RedirectableConn) error
}

// Router routes client connections to backends.
type Router interface {
	// ConnEventReceiver handles connection events to balance connections if possible.
	ConnEventReceiver

	GetBackendSelector() BackendSelector
	RefreshBackend()
	RedirectConnections() error
	ConnCount() int
	// ServerVersion returns the TiDB version.
	ServerVersion() string
	Close()
}

type connPhase int

const (
	// The session is never redirected.
	phaseNotRedirected connPhase = iota
	// The session is redirecting.
	phaseRedirectNotify
	// The session redirected successfully last time.
	phaseRedirectEnd
	// The session failed to redirect last time.
	phaseRedirectFail
)

const (
	// The interval to rebalance connections.
	rebalanceInterval = 10 * time.Millisecond
	// The number of connections to rebalance during each interval.
	// Limit the number to avoid creating too many connections suddenly on a backend.
	rebalanceConnsPerLoop = 10
	// The threshold of ratio of the highest score and lowest score.
	// If the ratio exceeds the threshold, the proxy will rebalance connections.
	rebalanceMaxScoreRatio = 1.2
	// After a connection fails to redirect, it may contain some unmigratable status.
	// Limit its redirection interval to avoid unnecessary retrial to reduce latency jitter.
	redirectFailMinInterval = 3 * time.Second
)

// RedirectableConn indicates a redirect-able connection.
type RedirectableConn interface {
	SetEventReceiver(receiver ConnEventReceiver)
	SetValue(key, val any)
	Value(key any) any
	// Redirect returns false if the current conn is not redirectable.
	Redirect(addr string) bool
	NotifyBackendStatus(status BackendStatus)
	ConnectionID() uint64
}

// backendWrapper contains the connections on the backend.
type backendWrapper struct {
	*backendHealth
	addr string
	// connScore is used for calculating backend scores and check if the backend can be removed from the list.
	// connScore = connList.Len() + incoming connections - outgoing connections.
	connScore int
	// A list of *connWrapper and is ordered by the connecting or redirecting time.
	// connList only includes the connections that are currently on this backend.
	connList *glist.List[*connWrapper]
}

// score calculates the score of the backend. Larger score indicates higher load.
func (b *backendWrapper) score() int {
	return b.status.ToScore() + b.connScore
}

// connWrapper wraps RedirectableConn.
type connWrapper struct {
	RedirectableConn
	phase connPhase
	// Last redirect start time of this connection.
	lastRedirect time.Time
}
