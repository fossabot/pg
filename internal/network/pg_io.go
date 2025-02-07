// Copyright 2019 MQ, Inc. All rights reserved.
//
// Use of this source code is governed by a MIT license
// that can be found in the LICENSE file in the root of the source
// tree.

package network

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/tls"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"github.com/blusewang/pg/internal/helper"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

func NewPgIO(dsn *helper.DataSourceName) *PgIO {
	pi := new(PgIO)
	pi.dsn = dsn
	pi.ServerConf = make(map[string]string)
	pi.IOError = nil
	return pi
}

type PgIO struct {
	dsn        *helper.DataSourceName
	tlsConfig  tls.Config
	conn       net.Conn
	reader     *bufio.Reader
	txStatus   TransactionStatus
	serverPid  uint32
	ServerConf map[string]string
	backendKey uint32
	Location   *time.Location
	IOError    error
}

func (pi *PgIO) Md5(s string) string {
	h := md5.New()
	h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (pi *PgIO) receivePgMsg(sep Identifies) (ms []PgMessage, err error) {
	for {
		var msg PgMessage
		id, err := pi.reader.ReadByte()
		if err != nil {
			pi.IOError = err
			return ms, err
		}
		msg.Identifies = Identifies(id)
		msg.Content, err = pi.reader.Peek(4)
		if err != nil {
			return ms, err
		}
		msg.Len = binary.BigEndian.Uint32(msg.Content)
		msg.Content = make([]byte, msg.Len, msg.Len)
		_, err = io.ReadFull(pi.reader, msg.Content)
		if err != nil {
			return ms, err
		}
		msg.Position = 4
		ms = append(ms, msg)
		if msg.Identifies == sep {
			return ms, nil
		}
	}
}

func (pi *PgIO) receivePgMsgOnce() (msg PgMessage, err error) {
	id, err := pi.reader.ReadByte()
	if err != nil {
		pi.IOError = err
		return msg, err
	}
	msg.Identifies = Identifies(id)
	msg.Content, err = pi.reader.Peek(4)
	if err != nil {
		return msg, err
	}
	msg.Len = binary.BigEndian.Uint32(msg.Content)
	msg.Content = make([]byte, msg.Len, msg.Len)
	_, err = io.ReadFull(pi.reader, msg.Content)
	if err != nil {
		return msg, err
	}
	msg.Position = 4
	if msg.Identifies == IdentifiesErrorResponse {
		return msg, msg.ParseError()
	}
	return
}

func (pi *PgIO) send(list ...*PgMessage) (err error) {
	var raw []byte
	for _, v := range list {
		raw = append(raw, v.encode()...)
	}
	_, pi.IOError = pi.conn.Write(raw)
	return pi.IOError
}

func (pi *PgIO) Dial(network, address string, timeout time.Duration) (err error) {
	pi.conn, err = net.DialTimeout(network, address, timeout)
	if err == nil {
		pi.reader = bufio.NewReader(pi.conn)
	}
	return
}

func (pi *PgIO) DialContext(context context.Context, network, address string, timeout time.Duration) (err error) {
	d := net.Dialer{Timeout: timeout}
	pi.conn, err = d.DialContext(context, network, address)
	if err == nil {
		pi.reader = bufio.NewReader(pi.conn)
	}
	return
}

func (pi *PgIO) StartUp() (err error) {
	if pi.dsn.SSL.Mode != "disable" && pi.dsn.SSL.Mode != "allow" {
		err = pi.ssl()
		if err != nil {
			return
		}
	}

	bs := NewPgMessage(IdentifiesStartupMessage)
	bs.addInt32(196608)
	for k, v := range pi.dsn.Parameter {
		bs.addString(k)
		bs.addString(v)
	}
	bs.addByte(0)
	_ = bs.encode()
	_, err = pi.conn.Write(bs.Content)
	if err != nil {
		return
	}

	for {
		m, err := pi.receivePgMsgOnce()
		if err != nil {
			return err
		}
		switch m.Identifies {
		case IdentifiesAuth:
			err = pi.auth(m)
			if err != nil {
				return err
			}
		case IdentifiesParameterStatus:
			k := m.string()
			v := m.string()
			if k == "TimeZone" {
				pi.Location, err = time.LoadLocation(v)
				if err != nil {
					err = nil
					pi.Location = nil
				}
			}
			pi.ServerConf[k] = v
		case IdentifiesBackendKeyData:
			pi.serverPid = m.int32()
			pi.backendKey = m.int32()
		case IdentifiesReadyForQuery:
			pi.txStatus = TransactionStatus(m.byte())
			return nil
		}
	}
}

func (pi *PgIO) auth(msg PgMessage) (err error) {
	switch code := msg.int32(); code {
	case 0:
		// OK
		break
	case 3:
		// 明文密码
		pwdMsg := NewPgMessage(IdentifiesPasswordMessage)
		pwdMsg.addString(pi.dsn.Password)
		err = pi.send(pwdMsg)
		if err != nil {
			return err
		}
		list, err := pi.receivePgMsg(IdentifiesAuth)
		if err != nil {
			return err
		}
		for _, v := range list {
			if v.Identifies == IdentifiesAuth && v.int32() != 0 {
				return fmt.Errorf("unexpected authentication response: %q", v.Identifies)
			}
		}

	case 5:
		// MD5密码
		reqPwd := NewPgMessage(IdentifiesPasswordMessage)
		reqPwd.addString("md5" + pi.Md5(pi.Md5(pi.dsn.Password+pi.dsn.Parameter["user"])+string(msg.bytes(4))))

		err = pi.send(reqPwd)
		if err != nil {
			return err
		}
		list, err := pi.receivePgMsg(IdentifiesAuth)
		if err != nil {
			return err
		}
		for _, v := range list {
			if v.Identifies == IdentifiesAuth && v.int32() != 0 {
				return fmt.Errorf("unexpected authentication response: %q", v.Identifies)
			}
		}
	}
	return
}

func (pi *PgIO) QueryNoArgs(query string) (cols []PgColumn, fieldLen *[][]uint32, data *[][][]byte, err error) {
	sq := NewPgMessage(IdentifiesQuery)
	sq.addString(query)
	err = pi.send(sq)
	if err != nil {
		return
	}

	fieldLen = new([][]uint32)
	data = new([][][]byte)

	list, err := pi.receivePgMsg(IdentifiesReadyForQuery)
	if err != nil {
		return
	}
	for _, v := range list {
		switch v.Identifies {
		case IdentifiesErrorResponse:
			err = v.ParseError()
		case IdentifiesDataRow:
			var rowLen = new([]uint32)
			var row = new([][]byte)
			length := v.int16()
			for i := uint16(0); i < length; i++ {
				l := v.int32()
				if l == 4294967295 {
					// nil
					*row = append(*row, nil)
				} else {
					*row = append(*row, v.bytes(l))
				}
				*rowLen = append(*rowLen, l)
			}
			*fieldLen = append(*fieldLen, *rowLen)
			*data = append(*data, *row)
		case IdentifiesRowDescription:
			cols = v.columns()
		case IdentifiesReadyForQuery:
			pi.txStatus = TransactionStatus(v.byte())
		}
	}
	return
}

func (pi *PgIO) Parse(name, query string) (cols []PgColumn, parameters []uint32, err error) {
	reqParse := NewPgMessage(IdentifiesParse)
	reqParse.addString(name)
	reqParse.addString(query)
	reqParse.addInt16(0) // 参数数量统一置0

	reqDes := NewPgMessage(IdentifiesDescribe)
	reqDes.addByte('S')
	reqDes.addString(name)

	err = pi.send(reqParse, reqDes, NewPgMessage(IdentifiesSync))
	if err != nil {
		return
	}

	list, err := pi.receivePgMsg(IdentifiesReadyForQuery)

	if err != nil {
		return
	}
	for _, v := range list {
		switch v.Identifies {
		case IdentifiesErrorResponse:
			err = v.ParseError()
		case IdentifiesParameterDescription:
			var pn = v.int16()
			for i := uint16(0); i < pn; i++ {
				parameters = append(parameters, v.int32())
			}
		case IdentifiesRowDescription:
			cols = v.columns()
		case IdentifiesReadyForQuery:
			pi.txStatus = TransactionStatus(v.byte())
		}
	}
	return
}

func (pi *PgIO) ParseExec(name string, args []interface{}) (n int, err error) {
	rBind := NewPgMessage(IdentifiesBind)
	rBind.addString("")
	rBind.addString(name)
	rBind.addInt16(0)
	rBind.addInt16(len(args))
	for _, arg := range args {
		if arg == nil {
			rBind.addInt32(-1)
		} else {
			b := value2bytes(arg)
			rBind.addInt32(len(b))
			rBind.addBytes(b)
		}
	}
	rBind.addInt16(0)
	rExec := NewPgMessage(IdentifiesExecute)
	rExec.addString("")
	rExec.addInt32(0) // all rows
	err = pi.send(rBind, rExec, NewPgMessage(IdentifiesSync))
	if err != nil {
		return
	}
	list, err := pi.receivePgMsg(IdentifiesReadyForQuery)
	if err != nil {
		return
	}
	for _, v := range list {
		switch v.Identifies {
		case IdentifiesErrorResponse:
			err = v.ParseError()
		case IdentifiesCommandComplete:
			var rs = strings.Split(v.string(), " ")
			if len(rs) == 2 {
				n, _ = strconv.Atoi(rs[1])
			}
		case IdentifiesReadyForQuery:
			pi.txStatus = TransactionStatus(v.byte())
		}
	}
	return
}

// data 使用指针减少copy时的内存损耗
func (pi *PgIO) ParseQuery(name string, args []interface{}) (fieldLen *[][]uint32, data *[][][]byte, err error) {
	rBind := NewPgMessage(IdentifiesBind)
	rBind.addString("")
	rBind.addString(name)
	rBind.addInt16(0)
	rBind.addInt16(len(args))
	for _, arg := range args {
		if arg == nil {
			rBind.addInt32(-1)
		} else {
			b := value2bytes(arg)
			rBind.addInt32(len(b))
			rBind.addBytes(b)
		}
	}
	rBind.addInt16(0)
	rExec := NewPgMessage(IdentifiesExecute)
	rExec.addString("")
	rExec.addInt32(0) // all rows
	err = pi.send(rBind, rExec, NewPgMessage(IdentifiesSync))
	if err != nil {
		return
	}
	list, err := pi.receivePgMsg(IdentifiesReadyForQuery)
	if err != nil {
		return
	}
	fieldLen = new([][]uint32)
	data = new([][][]byte)

	for _, v := range list {
		switch v.Identifies {
		case IdentifiesErrorResponse:
			err = v.ParseError()
		case IdentifiesDataRow:
			var rowLen = new([]uint32)
			var row = new([][]byte)
			length := v.int16()
			for i := uint16(0); i < length; i++ {
				l := v.int32()
				if l == 4294967295 {
					// nil
					*row = append(*row, nil)
				} else {
					*row = append(*row, v.bytes(l))
				}
				*rowLen = append(*rowLen, l)
			}
			*fieldLen = append(*fieldLen, *rowLen)
			*data = append(*data, *row)
		case IdentifiesReadyForQuery:
			pi.txStatus = TransactionStatus(v.byte())
		}
	}
	return
}

func (pi *PgIO) CloseParse(name string) (err error) {
	rc := NewPgMessage(IdentifiesClose)
	rc.addByte('S')
	rc.addString(name)

	err = pi.send(rc, NewPgMessage(IdentifiesSync))
	if err != nil {
		return
	}
	list, err := pi.receivePgMsg(IdentifiesReadyForQuery)
	if err != nil {
		return
	}
	for _, v := range list {
		if v.Identifies == IdentifiesReadyForQuery {
			pi.txStatus = TransactionStatus(v.byte())
		} else if v.Identifies == IdentifiesErrorResponse {
			err = v.ParseError()
		}
	}
	return
}

func (pi *PgIO) CancelRequest() (err error) {
	var nIO = NewPgIO(pi.dsn)
	err = nIO.Dial(pi.dsn.Address())
	if err != nil {
		return
	}
	rc := NewPgMessage(IdentifiesCancelRequest)
	rc.addInt32(80877102)
	rc.addInt32(int(pi.serverPid))
	rc.addInt32(int(pi.backendKey))

	_ = rc.encode()
	_, err = nIO.conn.Write(rc.Content)
	if err != nil {
		return
	}
	defer nIO.conn.Close()
	return
}

func (pi *PgIO) Terminate() (err error) {
	rc := NewPgMessage(IdentifiesTerminate)
	err = pi.send(rc)
	if err != nil {
		_ = pi.conn.Close()
		pi.IOError = driver.ErrBadConn
	}
	return
}

func (pi *PgIO) IsInTransaction() bool {
	return pi.txStatus == TransactionStatusIdleInTransaction || pi.txStatus == TransactionStatusInFailedTransaction
}
