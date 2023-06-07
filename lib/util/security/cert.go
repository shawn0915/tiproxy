// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package security

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"sync/atomic"
	"time"

	"github.com/pingcap/TiProxy/lib/config"
	"github.com/pingcap/TiProxy/lib/util/errors"
	"go.uber.org/zap"
)

const (
	// Recreate the auto certs one hour before it expires.
	// It should be longer than defaultRetryInterval.
	recreateAutoCertAdvance = 24 * time.Hour
)

var emptyCert = new(tls.Certificate)

type CertInfo struct {
	cfg         atomic.Pointer[config.TLSConfig]
	ca          atomic.Pointer[x509.CertPool]
	cert        atomic.Pointer[tls.Certificate]
	autoCertExp atomic.Int64
	server      bool
}

func NewCert(server bool) *CertInfo {
	return &CertInfo{
		server: server,
	}
}

func (ci *CertInfo) Reload(lg *zap.Logger) (tlsConfig *tls.Config, err error) {
	// Some methods to rotate server config:
	// - For certs: customize GetCertificate / GetConfigForClient.
	// - For CA: customize ClientAuth + VerifyPeerCertificate / GetConfigForClient
	// Some methods to rotate client config:
	// - For certs: customize GetClientCertificate
	// - For CA: customize InsecureSkipVerify + VerifyPeerCertificate
	if ci.server {
		tlsConfig, err = ci.buildServerConfig(lg)
	} else {
		tlsConfig, err = ci.buildClientConfig(lg)
	}
	return tlsConfig, err
}

func (ci *CertInfo) SetConfig(cfg config.TLSConfig) {
	ci.cfg.Store(&cfg)
}

func (ci *CertInfo) getCert(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return ci.cert.Load(), nil
}

func (ci *CertInfo) getClientCert(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	cert := ci.cert.Load()
	if cert == nil {
		// GetClientCertificate must return a non-nil Certificate.
		return emptyCert, nil
	}
	return cert, nil
}

func (ci *CertInfo) verifyPeerCertificate(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return nil
	}

	certs := make([]*x509.Certificate, len(rawCerts))
	for i, asn1Data := range rawCerts {
		cert, err := x509.ParseCertificate(asn1Data)
		if err != nil {
			return errors.New("tls: failed to parse certificate from server: " + err.Error())
		}
		certs[i] = cert
	}

	cas := ci.ca.Load()
	if cas == nil {
		cas = x509.NewCertPool()
	}
	opts := x509.VerifyOptions{
		Roots:         cas,
		Intermediates: x509.NewCertPool(),
	}
	if ci.server {
		opts.KeyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		// this is the default behavior of Verify()
		// it is not necessary but explicit
		opts.KeyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	// TODO: not implemented, maybe later
	// opts.DNSName = ci.serverName
	for _, cert := range certs[1:] {
		opts.Intermediates.AddCert(cert)
	}
	_, err := certs[0].Verify(opts)
	return err
}

func (ci *CertInfo) loadCA(pemCerts []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	for len(pemCerts) > 0 {
		var block *pem.Block
		block, pemCerts = pem.Decode(pemCerts)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}

		certBytes := block.Bytes
		cert, err := x509.ParseCertificate(certBytes)
		if err != nil {
			continue
		}
		pool.AddCert(cert)
	}
	return pool, nil
}

func (ci *CertInfo) buildServerConfig(lg *zap.Logger) (*tls.Config, error) {
	lg = lg.With(zap.String("tls", "server"), zap.Any("cfg", ci.cfg.Load()))
	autoCerts := false
	cfg := ci.cfg.Load()
	if !cfg.HasCert() {
		if cfg.AutoCerts {
			autoCerts = true
		} else {
			lg.Info("require certificates to secure clients connections, disable TLS")
			return nil, nil
		}
	}

	tcfg := &tls.Config{
		MinVersion:            cfg.MinTLSVer(),
		GetCertificate:        ci.getCert,
		GetClientCertificate:  ci.getClientCert,
		VerifyPeerCertificate: ci.verifyPeerCertificate,
	}

	var certPEM, keyPEM []byte
	var err error
	if autoCerts {
		now := time.Now()
		if time.Unix(ci.autoCertExp.Load(), 0).Before(now) {
			dur, err := time.ParseDuration(cfg.AutoExpireDuration)
			if err != nil {
				dur = DefaultCertExpiration
			}
			ci.autoCertExp.Store(now.Add(DefaultCertExpiration - recreateAutoCertAdvance).Unix())
			certPEM, keyPEM, _, err = createTempTLS(cfg.RSAKeySize, dur)
			if err != nil {
				return nil, err
			}
		}
	} else {
		certPEM, err = os.ReadFile(cfg.Cert)
		if err != nil {
			return nil, err
		}
		keyPEM, err = os.ReadFile(cfg.Key)
		if err != nil {
			return nil, err
		}
	}

	if certPEM != nil {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		ci.cert.Store(&cert)
	}

	if !cfg.HasCA() {
		lg.Info("no CA, server will not authenticate clients (connection is still secured)")
		return tcfg, nil
	}

	caPEM, err := os.ReadFile(cfg.CA)
	if err != nil {
		return nil, err
	}

	cas, err := ci.loadCA(caPEM)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	ci.ca.Store(cas)

	if cfg.SkipCA {
		tcfg.ClientAuth = tls.RequestClientCert
	} else {
		tcfg.ClientAuth = tls.RequireAnyClientCert
	}

	return tcfg, nil
}

func (ci *CertInfo) buildClientConfig(lg *zap.Logger) (*tls.Config, error) {
	lg = lg.With(zap.String("tls", "client"), zap.Any("cfg", ci.cfg.Load()))
	cfg := ci.cfg.Load()
	if cfg.AutoCerts {
		lg.Info("specified auto-certs in a client tls config, ignored")
	}

	if !cfg.HasCA() {
		if cfg.SkipCA {
			// still enable TLS without verify server certs
			return &tls.Config{
				InsecureSkipVerify: true,
				MinVersion:         cfg.MinTLSVer(),
			}, nil
		}
		lg.Info("no CA to verify server connections, disable TLS")
		return nil, nil
	}

	tcfg := &tls.Config{
		MinVersion:            cfg.MinTLSVer(),
		GetCertificate:        ci.getCert,
		GetClientCertificate:  ci.getClientCert,
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: ci.verifyPeerCertificate,
	}

	certBytes, err := os.ReadFile(cfg.CA)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	cas, err := ci.loadCA(certBytes)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	ci.ca.Store(cas)

	if !cfg.HasCert() {
		lg.Info("no certificates, server may reject the connection")
		return tcfg, nil
	}

	cert, err := tls.LoadX509KeyPair(cfg.Cert, cfg.Key)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	ci.cert.Store(&cert)

	return tcfg, nil
}
