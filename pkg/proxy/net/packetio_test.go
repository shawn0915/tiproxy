// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/pingcap/TiProxy/lib/config"
	"github.com/pingcap/TiProxy/lib/util/logger"
	"github.com/pingcap/TiProxy/lib/util/security"
	"github.com/pingcap/TiProxy/pkg/testkit"
	"github.com/stretchr/testify/require"
)

func testPipeConn(t *testing.T, a func(*testing.T, *PacketIO), b func(*testing.T, *PacketIO), loop int) {
	lg, _ := logger.CreateLoggerForTest(t)
	testkit.TestPipeConn(t,
		func(t *testing.T, c net.Conn) {
			a(t, NewPacketIO(c, lg))
		},
		func(t *testing.T, c net.Conn) {
			b(t, NewPacketIO(c, lg))
		}, loop)
}

func testTCPConn(t *testing.T, a func(*testing.T, *PacketIO), b func(*testing.T, *PacketIO), loop int) {
	lg, _ := logger.CreateLoggerForTest(t)
	testkit.TestTCPConn(t,
		func(t *testing.T, c net.Conn) {
			cli := NewPacketIO(c, lg)
			a(t, cli)
			require.NoError(t, cli.Close())
		},
		func(t *testing.T, c net.Conn) {
			srv := NewPacketIO(c, lg)
			b(t, srv)
			require.NoError(t, srv.Close())
		}, loop)
}

func TestPacketIO(t *testing.T) {
	expectMsg := []byte("test")
	pktLengths := []int{0, MaxPayloadLen + 212, MaxPayloadLen, MaxPayloadLen * 2}
	testPipeConn(t,
		func(t *testing.T, cli *PacketIO) {
			var err error

			// send anything
			require.NoError(t, cli.WritePacket(expectMsg, true))

			outBytes := len(expectMsg) + 4
			for _, l := range pktLengths {
				require.NoError(t, cli.WritePacket(make([]byte, l), true))
				outBytes += l + (l/(MaxPayloadLen)+1)*4
				require.Equal(t, uint64(outBytes), cli.OutBytes())
			}

			// skip handshake
			_, err = cli.ReadPacket()
			require.NoError(t, err)

			// send correct and wrong capability flags
			var hdr [32]byte
			binary.LittleEndian.PutUint32(hdr[:], ClientSSL.Uint32())
			err = cli.WritePacket(hdr[:], true)
			require.NoError(t, err)

			binary.LittleEndian.PutUint16(hdr[:], 0)
			err = cli.WritePacket(hdr[:], true)
			require.NoError(t, err)
		},
		func(t *testing.T, srv *PacketIO) {
			var salt [40]byte
			var msg []byte
			var err error

			// receive "test"
			msg, err = srv.ReadPacket()
			require.NoError(t, err)
			require.Equal(t, expectMsg, msg)

			inBytes := len(expectMsg) + 4
			for _, l := range pktLengths {
				msg, err = srv.ReadPacket()
				require.NoError(t, err)
				require.Equal(t, l, len(msg))
				inBytes += l + (l/(MaxPayloadLen)+1)*4
				require.Equal(t, uint64(inBytes), srv.InBytes())
			}

			// send handshake
			require.NoError(t, srv.WriteInitialHandshake(0, salt[:], AuthNativePassword, ServerVersion))
			// salt should not be long enough
			require.ErrorIs(t, srv.WriteInitialHandshake(0, make([]byte, 4), AuthNativePassword, ServerVersion), ErrSaltNotLongEnough)

			// expect correct and wrong capability flags
			_, isSSL, err := srv.ReadSSLRequestOrHandshakeResp()
			require.NoError(t, err)
			require.True(t, isSSL)
			_, isSSL, err = srv.ReadSSLRequestOrHandshakeResp()
			require.NoError(t, err)
			require.False(t, isSSL)
		},
		1,
	)
}

func TestTLS(t *testing.T) {
	stls, ctls, err := security.CreateTLSConfigForTest()
	require.NoError(t, err)
	message := []byte("hello world")
	testTCPConn(t,
		func(t *testing.T, cli *PacketIO) {
			data, err := cli.ReadPacket()
			require.NoError(t, err)
			require.Equal(t, message, data)
			err = cli.WritePacket(message, true)
			require.NoError(t, err)

			require.NoError(t, cli.ClientTLSHandshake(ctls))

			err = cli.WritePacket(message, true)
			require.NoError(t, err)
			data, err = cli.ReadPacket()
			require.NoError(t, err)
			require.Equal(t, message, data)
		},
		func(t *testing.T, srv *PacketIO) {
			err = srv.WritePacket(message, true)
			require.NoError(t, err)
			data, err := srv.ReadPacket()
			require.NoError(t, err)
			require.Equal(t, message, data)

			_, err = srv.ServerTLSHandshake(stls)
			require.NoError(t, err)

			data, err = srv.ReadPacket()
			require.NoError(t, err)
			require.Equal(t, message, data)
			err = srv.WritePacket(message, true)
			require.NoError(t, err)
		},
		500, // unable to reproduce stably, loop 500 times
	)
}

func TestPacketIOClose(t *testing.T) {
	testTCPConn(t,
		func(t *testing.T, cli *PacketIO) {
			require.NoError(t, cli.Close())
			require.NoError(t, cli.Close())
			require.NoError(t, cli.GracefulClose())
			require.NotEqual(t, cli.LocalAddr(), "")
			require.NotEqual(t, cli.RemoteAddr(), "")
		},
		func(t *testing.T, srv *PacketIO) {
			require.NoError(t, srv.GracefulClose())
			require.NoError(t, srv.Close())
			require.NoError(t, srv.Close())
			require.NotEqual(t, srv.LocalAddr(), "")
			require.NotEqual(t, srv.RemoteAddr(), "")
		},
		1,
	)
}

func TestPeerActive(t *testing.T) {
	stls, ctls, err := security.CreateTLSConfigForTest()
	require.NoError(t, err)
	ch := make(chan struct{})
	testTCPConn(t,
		func(t *testing.T, cli *PacketIO) {
			// It's active at the beginning.
			require.True(t, cli.IsPeerActive())
			ch <- struct{}{} // let srv write packet
			// ReadPacket still reads the whole data after checking.
			ch <- struct{}{}
			require.True(t, cli.IsPeerActive())
			data, err := cli.ReadPacket()
			require.NoError(t, err)
			require.Equal(t, "123", string(data))
			// IsPeerActive works after reading data.
			require.True(t, cli.IsPeerActive())
			// IsPeerActive works after writing data.
			require.NoError(t, cli.WritePacket([]byte("456"), true))
			require.True(t, cli.IsPeerActive())
			// upgrade to TLS and try again
			require.NoError(t, cli.ClientTLSHandshake(ctls))
			require.True(t, cli.IsPeerActive())
			data, err = cli.ReadPacket()
			require.NoError(t, err)
			require.Equal(t, "123", string(data))
			require.True(t, cli.IsPeerActive())
			require.NoError(t, cli.WritePacket([]byte("456"), true))
			require.True(t, cli.IsPeerActive())
			// It's not active after the peer closes.
			ch <- struct{}{}
			ch <- struct{}{}
			require.False(t, cli.IsPeerActive())
		},
		func(t *testing.T, srv *PacketIO) {
			<-ch
			err := srv.WritePacket([]byte("123"), true)
			require.NoError(t, err)
			<-ch
			data, err := srv.ReadPacket()
			require.NoError(t, err)
			require.Equal(t, "456", string(data))
			// upgrade to TLS and try again
			_, err = srv.ServerTLSHandshake(stls)
			require.NoError(t, err)
			err = srv.WritePacket([]byte("123"), true)
			require.NoError(t, err)
			data, err = srv.ReadPacket()
			require.NoError(t, err)
			require.Equal(t, "456", string(data))
			<-ch
			require.NoError(t, srv.Close())
			<-ch
		},
		10,
	)
}

func TestKeepAlive(t *testing.T) {
	stls, ctls, err := security.CreateTLSConfigForTest()
	require.NoError(t, err)
	frontend, backendHealthy, backendUnhealthy := config.DefaultKeepAlive()
	backendUnhealthy.Timeout = 2 * time.Second
	backendUnhealthy.Idle = time.Second
	backendUnhealthy.Cnt = 1
	backendUnhealthy.Intvl = time.Second
	testTCPConn(t,
		func(t *testing.T, cli *PacketIO) {
			require.NoError(t, cli.SetKeepalive(frontend))
			require.NoError(t, cli.ClientTLSHandshake(ctls))
			time.Sleep(3 * time.Second)
			_, err := cli.ReadPacket()
			require.NoError(t, err)
			require.NoError(t, cli.WritePacket([]byte{0, 1, 2}, true))
		},
		func(t *testing.T, srv *PacketIO) {
			require.NoError(t, srv.SetKeepalive(backendHealthy))
			_, err = srv.ServerTLSHandshake(stls)
			require.NoError(t, err)
			require.NoError(t, srv.SetKeepalive(backendUnhealthy))
			require.NoError(t, srv.WritePacket([]byte{0, 1, 2}, true))
			time.Sleep(3*time.Second + 100*time.Millisecond)
			_, err := srv.ReadPacket()
			require.NoError(t, err)
		},
		1,
	)
}
