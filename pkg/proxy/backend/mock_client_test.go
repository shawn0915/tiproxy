// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"crypto/tls"
	"encoding/binary"

	pnet "github.com/pingcap/TiProxy/pkg/proxy/net"
	"github.com/pingcap/tidb/parser/mysql"
)

type clientConfig struct {
	// for auth
	tlsConfig  *tls.Config
	sql        string
	username   string
	dbName     string
	authPlugin string
	attrs      map[string]string
	dataBytes  []byte
	authData   []byte
	filePkts   int
	prepStmtID int
	capability pnet.Capability
	collation  uint8
	cmd        pnet.Command
	// for both auth and cmd
	abnormalExit bool
}

func newClientConfig() *clientConfig {
	return &clientConfig{
		capability: defaultTestClientCapability,
		username:   mockUsername,
		dbName:     mockDBName,
		authPlugin: mysql.AuthCachingSha2Password,
		authData:   mockAuthData,
		attrs:      make(map[string]string),
		cmd:        pnet.ComQuery,
		dataBytes:  mockCmdBytes,
		sql:        mockCmdStr,
	}
}

type mockClient struct {
	err error
	// Inputs that assigned by the test and will be sent to the server.
	*clientConfig
	// Outputs that received from the server and will be checked by the test.
	authSucceed   bool
	mysqlErr      error
	serverVersion string
}

func newMockClient(cfg *clientConfig) *mockClient {
	return &mockClient{
		clientConfig: cfg,
	}
}

func (mc *mockClient) authenticate(packetIO *pnet.PacketIO) error {
	if mc.abnormalExit {
		return packetIO.Close()
	}
	pkt, err := packetIO.ReadPacket()
	if err != nil {
		return err
	}
	serverCap, serverVersion := pnet.ParseInitialHandshake(pkt)
	mc.capability = mc.capability & serverCap
	mc.serverVersion = serverVersion

	resp := &pnet.HandshakeResp{
		User:       mc.username,
		DB:         mc.dbName,
		AuthPlugin: mc.authPlugin,
		Attrs:      mc.attrs,
		AuthData:   mc.authData,
		Capability: mc.capability,
		Collation:  mc.collation,
	}
	pkt = pnet.MakeHandshakeResponse(resp)
	if mc.capability&pnet.ClientSSL > 0 {
		if err := packetIO.WritePacket(pkt[:32], true); err != nil {
			return err
		}
		if err := packetIO.ClientTLSHandshake(mc.tlsConfig); err != nil {
			return err
		}
	}
	if err := packetIO.WritePacket(pkt, true); err != nil {
		return err
	}
	return mc.writePassword(packetIO)
}

func (mc *mockClient) writePassword(packetIO *pnet.PacketIO) error {
	for {
		serverPkt, err := packetIO.ReadPacket()
		if err != nil {
			return err
		}
		switch serverPkt[0] {
		case mysql.OKHeader:
			mc.authSucceed = true
			return nil
		case mysql.ErrHeader:
			mc.authSucceed = false
			mc.mysqlErr = pnet.ParseErrorPacket(serverPkt)
			return nil
		case mysql.AuthSwitchRequest, pnet.ShaCommand:
			if err := packetIO.WritePacket(mc.authData, true); err != nil {
				return err
			}
		}
	}
}

// request sends commands except prepared statements commands.
func (mc *mockClient) request(packetIO *pnet.PacketIO) error {
	if mc.abnormalExit {
		return packetIO.Close()
	}
	packetIO.ResetSequence()
	data := []byte{mc.cmd.Byte()}
	switch mc.cmd {
	case pnet.ComInitDB, pnet.ComCreateDB, pnet.ComDropDB:
		data = append(data, []byte(mockCmdStr)...)
	case pnet.ComQuery:
		return mc.query(packetIO)
	case pnet.ComProcessInfo:
		return mc.requestProcessInfo(packetIO)
	case pnet.ComFieldList:
		return mc.requestFieldList(packetIO)
	case pnet.ComRefresh, pnet.ComSetOption:
		data = append(data, mc.dataBytes...)
	case pnet.ComProcessKill:
		data = pnet.DumpUint32(data, uint32(mockCmdInt))
	case pnet.ComChangeUser:
		return mc.requestChangeUser(packetIO)
	case pnet.ComStmtPrepare:
		return mc.requestPrepare(packetIO)
	case pnet.ComStmtSendLongData:
		data = pnet.DumpUint32(data, uint32(mc.prepStmtID))
		data = append(data, mc.dataBytes...)
	case pnet.ComStmtExecute:
		return mc.requestExecute(packetIO)
	case pnet.ComStmtFetch:
		return mc.requestFetch(packetIO)
	case pnet.ComStmtClose, pnet.ComStmtReset:
		data = pnet.DumpUint32(data, uint32(mc.prepStmtID))
	}
	if err := packetIO.WritePacket(data, true); err != nil {
		return err
	}
	switch mc.cmd {
	case pnet.ComQuit, pnet.ComStmtClose, pnet.ComStmtSendLongData:
		return nil
	}
	_, err := packetIO.ReadPacket()
	return err
}

func (mc *mockClient) requestChangeUser(packetIO *pnet.PacketIO) error {
	data := pnet.MakeChangeUser(mc.username, mc.dbName, mysql.AuthNativePassword, mc.authData)
	if err := packetIO.WritePacket(data, true); err != nil {
		return err
	}
	for {
		resp, err := packetIO.ReadPacket()
		if err != nil {
			return err
		}
		switch resp[0] {
		case mysql.OKHeader:
			return nil
		case mysql.ErrHeader:
			mc.mysqlErr = pnet.ParseErrorPacket(resp)
			return nil
		default:
			if err := packetIO.WritePacket(mc.authData, true); err != nil {
				return err
			}
		}
	}
}

func (mc *mockClient) requestPrepare(packetIO *pnet.PacketIO) error {
	data := make([]byte, 0, len(mc.sql)+1)
	data = append(data, pnet.ComStmtPrepare.Byte())
	data = append(data, []byte(mc.sql)...)
	if err := packetIO.WritePacket(data, true); err != nil {
		return err
	}
	response, err := packetIO.ReadPacket()
	if err != nil {
		return err
	}
	expectedPacketNum := 0
	if response[0] == mysql.OKHeader {
		numColumns := binary.LittleEndian.Uint16(response[5:])
		numParams := binary.LittleEndian.Uint16(response[7:])
		expectedPacketNum = int(numColumns) + int(numParams)
		if mc.capability&pnet.ClientDeprecateEOF == 0 {
			if numColumns > 0 {
				expectedPacketNum++
			}
			if numParams > 0 {
				expectedPacketNum++
			}
		}
	}
	for i := 0; i < expectedPacketNum; i++ {
		if _, err = packetIO.ReadPacket(); err != nil {
			return err
		}
	}
	return nil
}

func (mc *mockClient) requestExecute(packetIO *pnet.PacketIO) error {
	data := make([]byte, 0, len(mc.dataBytes)+5)
	data = append(data, pnet.ComStmtExecute.Byte())
	data = pnet.DumpUint32(data, uint32(mc.prepStmtID))
	data = append(data, mc.dataBytes...)
	if err := packetIO.WritePacket(data, true); err != nil {
		return err
	}
	return mc.readResultSet(packetIO)
}

func (mc *mockClient) requestFetch(packetIO *pnet.PacketIO) error {
	data := make([]byte, 0, len(mc.dataBytes)+5)
	data = append(data, pnet.ComStmtFetch.Byte())
	data = pnet.DumpUint32(data, uint32(mc.prepStmtID))
	data = append(data, mc.dataBytes...)
	if err := packetIO.WritePacket(data, true); err != nil {
		return err
	}
	_, err := mc.readUntilResultEnd(packetIO)
	return err
}

func (mc *mockClient) requestFieldList(packetIO *pnet.PacketIO) error {
	data := make([]byte, 0, len(mockCmdStr)+2)
	data = append(data, pnet.ComFieldList.Byte())
	data = append(data, []byte(mockCmdStr)...)
	data = append(data, 0x00)
	data = append(data, []byte(mockCmdStr)...)
	if err := packetIO.WritePacket(data, true); err != nil {
		return err
	}
	_, err := mc.readUntilResultEnd(packetIO)
	return err
}

func (mc *mockClient) readUntilResultEnd(packetIO *pnet.PacketIO) (pkt []byte, err error) {
	for {
		pkt, err = packetIO.ReadPacket()
		if err != nil {
			return
		}
		if pkt[0] == mysql.ErrHeader {
			mc.mysqlErr = pnet.ParseErrorPacket(pkt)
			return
		}
		if mc.capability&pnet.ClientDeprecateEOF == 0 {
			if pnet.IsEOFPacket(pkt) {
				break
			}
		} else {
			if pnet.IsResultSetOKPacket(pkt) {
				break
			}
		}
	}
	return
}

func (mc *mockClient) requestProcessInfo(packetIO *pnet.PacketIO) error {
	if err := packetIO.WritePacket([]byte{pnet.ComProcessInfo.Byte()}, true); err != nil {
		return err
	}
	return mc.readResultSet(packetIO)
}

func (mc *mockClient) query(packetIO *pnet.PacketIO) error {
	data := make([]byte, 0, len(mc.sql)+1)
	data = append(data, pnet.ComQuery.Byte())
	data = append(data, []byte(mc.sql)...)
	if err := packetIO.WritePacket(data, true); err != nil {
		return err
	}
	return mc.readResultSet(packetIO)
}

func (mc *mockClient) readResultSet(packetIO *pnet.PacketIO) error {
	for {
		var serverStatus uint16
		pkt, err := packetIO.ReadPacket()
		if err != nil {
			return err
		}
		switch pkt[0] {
		case mysql.OKHeader:
			serverStatus = binary.LittleEndian.Uint16(pkt[3:])
		case mysql.ErrHeader:
			mc.mysqlErr = pnet.ParseErrorPacket(pkt)
			return nil
		case mysql.LocalInFileHeader:
			for i := 0; i < mc.filePkts; i++ {
				if err = packetIO.WritePacket(mc.dataBytes, false); err != nil {
					return err
				}
			}
			if err = packetIO.WritePacket(nil, true); err != nil {
				return err
			}
			if pkt, err = packetIO.ReadPacket(); err != nil {
				return err
			}
			if pkt[0] == mysql.OKHeader {
				serverStatus = binary.LittleEndian.Uint16(pkt[3:])
			} else {
				return nil
			}
		default:
			// read result set
			if mc.capability&pnet.ClientDeprecateEOF == 0 {
				if pkt, err = mc.readUntilResultEnd(packetIO); err != nil {
					return err
				}
				if pkt[0] == mysql.ErrHeader {
					return nil
				}
				serverStatus = binary.LittleEndian.Uint16(pkt[3:])
				if serverStatus&mysql.ServerStatusCursorExists > 0 {
					break
				}
			}
			if pkt, err = mc.readUntilResultEnd(packetIO); err != nil {
				return err
			}
			if pkt[0] == mysql.ErrHeader {
				return nil
			}
			if mc.capability&pnet.ClientDeprecateEOF == 0 {
				serverStatus = binary.LittleEndian.Uint16(pkt[3:])
			} else {
				rs := pnet.ParseOKPacket(pkt)
				serverStatus = rs.Status
			}
		}
		if serverStatus&mysql.ServerMoreResultsExists == 0 {
			break
		}
	}
	return nil
}
