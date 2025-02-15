// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pingcap/TiProxy/lib/config"
	"github.com/pingcap/TiProxy/lib/util/errors"
	"github.com/pingcap/TiProxy/lib/util/waitgroup"
	mgrcrt "github.com/pingcap/TiProxy/pkg/manager/cert"
	mgrcfg "github.com/pingcap/TiProxy/pkg/manager/config"
	mgrns "github.com/pingcap/TiProxy/pkg/manager/namespace"
	"github.com/pingcap/TiProxy/pkg/proxy"
	"github.com/pingcap/TiProxy/pkg/proxy/proxyprotocol"
	"go.uber.org/atomic"
	"go.uber.org/ratelimit"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// DefAPILimit is the global API limit per second.
	DefAPILimit = 100
	// DefConnTimeout is used as timeout duration in the HTTP server.
	DefConnTimeout = 30 * time.Second
)

type HTTPHandler interface {
	RegisterHTTP(c *gin.Engine) error
}

type managers struct {
	cfg *mgrcfg.ConfigManager
	ns  *mgrns.NamespaceManager
	crt *mgrcrt.CertManager
}

type HTTPServer struct {
	listener net.Listener
	wg       waitgroup.WaitGroup
	limit    ratelimit.Limiter
	ready    *atomic.Bool
	lg       *zap.Logger
	proxy    *proxy.SQLServer
	mgr      managers
}

func NewHTTPServer(cfg config.API, lg *zap.Logger,
	proxy *proxy.SQLServer,
	nsmgr *mgrns.NamespaceManager, cfgmgr *mgrcfg.ConfigManager,
	crtmgr *mgrcrt.CertManager, handler HTTPHandler,
	ready *atomic.Bool) (*HTTPServer, error) {
	h := &HTTPServer{
		limit: ratelimit.New(DefAPILimit),
		ready: ready,
		lg:    lg,
		proxy: proxy,
		mgr:   managers{cfgmgr, nsmgr, crtmgr},
	}

	var err error
	h.listener, err = net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	switch cfg.ProxyProtocol {
	case "v2":
		h.listener = proxyprotocol.NewListener(h.listener)
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(
		gin.Recovery(),
		h.rateLimit,
		h.attachLogger,
		h.readyState,
	)

	h.register(engine.Group("/api"), cfg, nsmgr, cfgmgr)

	if handler != nil {
		if err := handler.RegisterHTTP(engine); err != nil {
			return nil, errors.WithStack(err)
		}
	}

	if tlscfg := crtmgr.ServerTLS(); tlscfg != nil {
		h.listener = tls.NewListener(h.listener, tlscfg)
	}

	hsrv := http.Server{
		Handler:           engine.Handler(),
		ReadHeaderTimeout: DefConnTimeout,
		IdleTimeout:       DefConnTimeout,
	}

	h.wg.Run(func() {
		lg.Info("HTTP closed", zap.Error(hsrv.Serve(h.listener)))
	})

	return h, nil
}

func (h *HTTPServer) rateLimit(c *gin.Context) {
	_ = h.limit.Take()
}

func (h *HTTPServer) attachLogger(c *gin.Context) {
	path := c.Request.URL.Path

	fields := make([]zapcore.Field, 0, 9)

	fields = append(fields,
		zap.Int("status", c.Writer.Status()),
		zap.String("method", c.Request.Method),
		zap.String("path", path),
		zap.String("query", c.Request.URL.RawQuery),
		zap.String("ip", c.ClientIP()),
		zap.String("user-agent", c.Request.UserAgent()),
	)

	start := time.Now().UTC()
	c.Next()
	end := time.Now().UTC()
	latency := end.Sub(start)

	fields = append(fields,
		zap.Duration("latency", latency),
		zap.String("time", end.Format("")),
	)

	if len(c.Errors) > 0 {
		errs := make([]error, 0, len(c.Errors))
		for _, e := range c.Errors {
			errs = append(errs, e)
		}
		fields = append(fields, zap.Errors("errs", errs))
	}

	if len(c.Errors) > 0 {
		h.lg.Warn(path, fields...)
	} else if strings.HasPrefix(path, "/api/debug") || strings.HasPrefix(path, "/api/metrics") {
		h.lg.Debug(path, fields...)
	} else {
		h.lg.Info(path, fields...)
	}
}

func (h *HTTPServer) readyState(c *gin.Context) {
	if !h.ready.Load() {
		c.Abort()
		c.JSON(http.StatusInternalServerError, "service not ready")
	}
}

func (h *HTTPServer) register(group *gin.RouterGroup, cfg config.API, nsmgr *mgrns.NamespaceManager, cfgmgr *mgrcfg.ConfigManager) {
	{
		adminGroup := group.Group("admin")
		if cfg.EnableBasicAuth {
			adminGroup.Use(gin.BasicAuth(gin.Accounts{cfg.User: cfg.Password}))
		}
		h.registerNamespace(adminGroup.Group("namespace"))
		h.registerConfig(adminGroup.Group("config"))
	}

	h.registerMetrics(group.Group("metrics"))
	h.registerDebug(group.Group("debug"))
}

func (h *HTTPServer) Close() error {
	err := h.listener.Close()
	h.wg.Wait()
	return err
}
