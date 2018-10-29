package server

import (
	"bufio"
	"bytes"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"time"
)

import (
	log "github.com/AlexStocks/log4go"
	jerrors "github.com/juju/errors"
)

const (
	DefaultMaxSleepTime           = 1 * time.Second  // accept中间最大sleep interval
	DefaultMAXConnNum             = 50 * 1024 * 1024 // 默认最大连接数 50w
	DefaultHTTPRspBufferSize      = 1024
	PathPrefix               byte = byte('/')
)

type Package struct {
	Header map[string]string
	Body   []byte
}

func (m *Package) Reset() {
	m.Body = m.Body[:0]
	for key := range m.Header {
		delete(m.Header, key)
	}
}

//////////////////////////////////////////////
// http transport socket
//////////////////////////////////////////////

// 从下面代码来看，socket是为下面的listener服务的
type httpTransportSocket struct {
	conn net.Conn
	once *sync.Once
	sync.Mutex
	bufReader *bufio.Reader
	rspBuf    *bytes.Buffer
}

const (
	REQ_Q_SIZE = 1 // http1.1形式的短连接，一次也只能处理一个请求，放大size无意义
)

func InitHTTPTransportSocket(c net.Conn) *httpTransportSocket {
	return &httpTransportSocket{
		conn:      c,
		once:      &sync.Once{},
		bufReader: bufio.NewReader(c),
		rspBuf:    bytes.NewBuffer(make([]byte, DefaultHTTPRspBufferSize)),
	}
}

func SetNetConnTimeout(conn net.Conn, timeout time.Duration) {
	t := time.Time{}
	if timeout > time.Duration(0) {
		t = time.Now().Add(timeout)
	}

	conn.SetReadDeadline(t)
}

func (h *httpTransportSocket) Recv(p *Package) error {
	if p == nil {
		return jerrors.New("message passed in is nil")
	}

	// set timeout if its greater than 0
	SetNetConnTimeout(h.conn, 3e9)
	defer SetNetConnTimeout(h.conn, 0)

	r, err := http.ReadRequest(h.bufReader)
	if err != nil {
		return jerrors.Trace(err)
	}

	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return jerrors.Trace(err)
	}
	r.Body.Close()

	// 初始化的时候创建了Header，并给Body赋值
	mr := &Package{
		Header: make(map[string]string),
		Body:   b,
	}

	// 下面的代码块给Package{Header}进行赋值
	for k, v := range r.Header {
		if len(v) > 0 {
			mr.Header[k] = v[0]
		} else {
			mr.Header[k] = ""
		}
	}
	mr.Header["Path"] = r.URL.Path[1:] // to get service name
	if r.URL.Path[0] != PathPrefix {
		mr.Header["Path"] = r.URL.Path
	}
	mr.Header["HttpMethod"] = r.Method

	*p = *mr
	return nil
}

func (h *httpTransportSocket) Send(p *Package) error {
	rsp := &http.Response{
		Body:          ioutil.NopCloser(bytes.NewReader(p.Body)),
		Status:        "200 OK",
		StatusCode:    200,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: int64(len(p.Body)),
	}

	// 根据@p，修改Response{Header}
	for k, v := range p.Header {
		rsp.Header.Set(k, v)
	}

	// return rsp.Write(h.conn)
	h.rspBuf.Reset()
	err := rsp.Write(h.rspBuf)
	if err != nil {
		return jerrors.Trace(err)
	}

	// set timeout if its greater than 0
	SetNetConnTimeout(h.conn, 3e9)
	defer SetNetConnTimeout(h.conn, 0)

	_, err = h.rspBuf.WriteTo(h.conn)

	return jerrors.Trace(err)
}

func (h *httpTransportSocket) error(p *Package) error {
	b := bytes.NewBuffer(p.Body)
	defer b.Reset()
	rsp := &http.Response{
		Header:        make(http.Header),
		Body:          ioutil.NopCloser(bytes.NewReader(p.Body)),
		Status:        "500 Internal Server Error",
		StatusCode:    500,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: int64(len(p.Body)),
	}

	for k, v := range p.Header {
		rsp.Header.Set(k, v)
	}

	// return rsp.Write(h.conn)
	h.rspBuf.Reset()
	err := rsp.Write(h.rspBuf)
	if err != nil {
		return jerrors.Trace(err)
	}

	_, err = h.rspBuf.WriteTo(h.conn)
	return jerrors.Trace(err)
}

func (h *httpTransportSocket) Close() error {
	log.Debug("httpTransportSocket.Close")
	var err error
	h.once.Do(func() {
		h.Lock()
		h.bufReader.Reset(nil)
		h.bufReader = nil
		h.rspBuf.Reset()
		h.Unlock()
		err = h.conn.Close()
	})
	return err
}

func (h *httpTransportSocket) LocalAddr() net.Addr {
	return h.conn.LocalAddr()
}

func (h *httpTransportSocket) RemoteAddr() net.Addr {
	return h.conn.RemoteAddr()
}
