package server

import (
	"bytes"

	"github.com/dubbo/dubbo-go/jsonrpc"
)

type rpcCodec struct {
	socket *httpTransportSocket
	codec  *jsonrpc.ServerCodec

	pkg *Package
	buf *readWriteCloser
}

type readWriteCloser struct {
	wbuf *bytes.Buffer
	rbuf *bytes.Buffer
}

func (rwc *readWriteCloser) Read(p []byte) (n int, err error) {
	return rwc.rbuf.Read(p)
}

func (rwc *readWriteCloser) Write(p []byte) (n int, err error) {
	return rwc.wbuf.Write(p)
}

func (rwc *readWriteCloser) Close() error {
	rwc.rbuf.Reset()
	rwc.wbuf.Reset()
	return nil
}

func newRPCCodec(req *Package, socket *httpTransportSocket) serverCodec {
	rwc := &readWriteCloser{
		rbuf: bytes.NewBuffer(req.Body),
		wbuf: bytes.NewBuffer(nil),
	}
	r := &rpcCodec{
		buf:    rwc,
		pkg:    req,
		socket: socket,
		codec:  jsonrpc.NewServerCodec(rwc),
	}
	return r
}

func (c *rpcCodec) ReadRequestHeader(r *request) error {
	m := jsonrpc.Message{Header: c.pkg.Header}

	err := c.codec.ReadHeader(&m)
	r.Service = m.Target
	r.Method = m.Method
	r.Seq = m.ID
	return err
}

func (c *rpcCodec) ReadRequestBody(b interface{}) error {
	return c.codec.ReadBody(b)
}

func (c *rpcCodec) WriteResponse(r *response, body interface{}, last bool) error {
	c.buf.wbuf.Reset()
	m := &jsonrpc.Message{
		Target: r.Service,
		Method: r.Method,
		ID:     r.Seq,
		Error:  r.Error,
		Header: map[string]string{},
	}
	if err := c.codec.Write(m, body); err != nil {
		return err
	}

	m.Header["Content-Type"] = c.pkg.Header["Content-Type"]
	return c.socket.Send(&Package{
		Header: m.Header,
		Body:   c.buf.wbuf.Bytes(),
	})
}

func (c *rpcCodec) Close() error {
	c.buf.Close()
	c.codec.Close()
	return c.socket.Close()
}
