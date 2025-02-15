// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package testkit

import (
	"net"
	"testing"

	"github.com/pingcap/TiProxy/lib/util/waitgroup"
	"github.com/stretchr/testify/require"
)

func TestPipeConn(t *testing.T, a, b func(*testing.T, net.Conn), loop int) {
	var wg waitgroup.WaitGroup
	cli, srv := net.Pipe()
	if ddl, ok := t.Deadline(); ok {
		require.NoError(t, cli.SetDeadline(ddl))
		require.NoError(t, srv.SetDeadline(ddl))
	}
	for i := 0; i < loop; i++ {
		wg.Run(func() {
			a(t, cli)
			require.NoError(t, cli.Close())
		})
		wg.Run(func() {
			b(t, srv)
			require.NoError(t, srv.Close())
		})
		wg.Wait()
	}
}

func TestTCPConn(t *testing.T, a, b func(*testing.T, net.Conn), loop int) {
	TestTCPConnWithListener(t, func(t *testing.T, network, addr string) net.Listener {
		ln, err := net.Listen(network, addr)
		require.NoError(t, err)
		return ln
	}, a, b, loop)
}

func TestTCPConnWithListener(t *testing.T, listen func(*testing.T, string, string) net.Listener, a, b func(*testing.T, net.Conn), loop int) {
	listener := listen(t, "tcp", "localhost:0")
	defer func() {
		require.NoError(t, listener.Close())
	}()
	var wg waitgroup.WaitGroup
	for i := 0; i < loop; i++ {
		wg.Run(func() {
			cli, err := net.Dial("tcp", listener.Addr().String())
			require.NoError(t, err)
			if ddl, ok := t.Deadline(); ok {
				require.NoError(t, cli.SetDeadline(ddl))
			}
			a(t, cli)
			if err := cli.Close(); err != nil {
				require.ErrorIs(t, err, net.ErrClosed)
			}
		})
		wg.Run(func() {
			srv, err := listener.Accept()
			require.NoError(t, err)
			if ddl, ok := t.Deadline(); ok {
				require.NoError(t, srv.SetDeadline(ddl))
			}
			b(t, srv)
			if err := srv.Close(); err != nil {
				require.ErrorIs(t, err, net.ErrClosed)
			}
		})
		wg.Wait()
	}
}
