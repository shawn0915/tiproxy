package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// PanicCounter measures the count of panics.
	PanicCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ModuleWeirProxy,
			Subsystem: LabelServer,
			Name:      "panic_total",
			Help:      "Counter of panic.",
		}, []string{LblCluster, LblType})

	QueryTotalCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ModuleWeirProxy,
			Subsystem: LabelServer,
			Name:      "query_total",
			Help:      "Counter of queries.",
		}, []string{LblCluster, LblType, LblResult})

	ExecuteErrorCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ModuleWeirProxy,
			Subsystem: LabelServer,
			Name:      "execute_error_total",
			Help:      "Counter of execute errors.",
		}, []string{LblCluster, LblType})

	ConnGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: ModuleWeirProxy,
			Subsystem: LabelServer,
			Name:      "connections",
			Help:      "Number of connections.",
		}, []string{LblCluster})

	EventStart        = "start"
	EventGracefulDown = "graceful_shutdown"
	// Eventkill occurs when the server.Kill() function is called.
	EventKill  = "kill"
	EventClose = "close"
)
