// Copyright (c) 2016 ~ 2018, Alex Stocks.
// Copyright (c) 2015 Alex Efros.
//
// The MIT License (MIT)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package jsonrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"

	jerrors "github.com/juju/errors"
)

const (
	MAX_JSONRPC_ID = 0x7FFFFFFF
	VERSION        = "2.0"
)

type Message struct {
	ID          int64
	Version     string
	ServicePath string // service path
	Target      string // Service
	Method      string
	Timeout     time.Duration // request timeout
	Error       string
	Header      map[string]string
	BodyLen     int
}

type CodecData struct {
	ID     int64
	Method string
	Args   interface{}
	Error  string
}

const (
	// Errors defined in the JSON-RPC spec. See
	// http://www.jsonrpc.org/specification#error_object.
	CodeParseError       = -32700
	CodeInvalidRequest   = -32600
	CodeMethodNotFound   = -32601
	CodeInvalidParams    = -32602
	CodeInternalError    = -32603
	codeServerErrorStart = -32099
	codeServerErrorEnd   = -32000
)

// rsponse Error
type Error struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func (e *Error) Error() string {
	buf, err := json.Marshal(e)
	if err != nil {
		msg, err := json.Marshal(err.Error())
		if err != nil {
			msg = []byte("jsonrpc2.Error: json.Marshal failed")
		}
		return fmt.Sprintf(`{"code":%d,"message":%s}`, -32001, string(msg))
	}
	return string(buf)
}

//////////////////////////////////////////
// json client codec
//////////////////////////////////////////

type clientRequest struct {
	Version string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int64       `json:"id"`
}

type clientResponse struct {
	Version string           `json:"jsonrpc"`
	ID      int64            `json:"id"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

func (r *clientResponse) reset() {
	r.Version = ""
	r.ID = 0
	r.Result = nil
}

type jsonClientCodec struct {
	// temporary work space
	req clientRequest
	rsp clientResponse

	pending map[int64]string
}

func newJsonClientCodec() *jsonClientCodec {
	return &jsonClientCodec{
		pending: make(map[int64]string),
	}
}

func (c *jsonClientCodec) Write(d *CodecData) ([]byte, error) {
	// If return error: it will be returned as is for this call.
	// Allow param to be only Array, Slice, Map or Struct.
	// When param is nil or uninitialized Map or Slice - omit "params".
	param := d.Args
	if param != nil {
		switch k := reflect.TypeOf(param).Kind(); k {
		case reflect.Map:
			if reflect.TypeOf(param).Key().Kind() == reflect.String {
				if reflect.ValueOf(param).IsNil() {
					param = nil
				}
			}
		case reflect.Slice:
			if reflect.ValueOf(param).IsNil() {
				param = nil
			}
		case reflect.Array, reflect.Struct:
		case reflect.Ptr:
			switch k := reflect.TypeOf(param).Elem().Kind(); k {
			case reflect.Map:
				if reflect.TypeOf(param).Elem().Key().Kind() == reflect.String {
					if reflect.ValueOf(param).Elem().IsNil() {
						param = nil
					}
				}
			case reflect.Slice:
				if reflect.ValueOf(param).Elem().IsNil() {
					param = nil
				}
			case reflect.Array, reflect.Struct:
			default:
				return nil, jerrors.New("unsupported param type: Ptr to " + k.String())
			}
		default:
			return nil, jerrors.New("unsupported param type: " + k.String())
		}
	}

	c.req.Version = "2.0"
	c.req.Method = d.Method
	c.req.Params = param
	c.req.ID = d.ID & MAX_JSONRPC_ID
	// can not use d.ID. otherwise you will get error: can not find method of response id 280698512
	c.pending[c.req.ID] = d.Method

	buf := bytes.NewBuffer(nil)
	defer buf.Reset()
	enc := json.NewEncoder(buf)
	if err := enc.Encode(&c.req); err != nil {
		return nil, jerrors.Trace(err)
	}

	return buf.Bytes(), nil
}

func (c *jsonClientCodec) Read(streamBytes []byte, x interface{}) error {
	c.rsp.reset()

	buf := bytes.NewBuffer(streamBytes)
	defer buf.Reset()
	dec := json.NewDecoder(buf)
	if err := dec.Decode(&c.rsp); err != nil {
		if err != io.EOF {
			err = jerrors.Trace(err)
		}
		return err
	}

	_, ok := c.pending[c.rsp.ID]
	if !ok {
		err := jerrors.Errorf("can not find method of rsponse id %v, rsponse error:%v", c.rsp.ID, c.rsp.Error)
		return err
	}
	delete(c.pending, c.rsp.ID)

	// c.rsp.ID
	if c.rsp.Error != nil {
		return jerrors.New(c.rsp.Error.Error())
	}

	return jerrors.Trace(json.Unmarshal(*c.rsp.Result, x))
}

//////////////////////////////////////////
// json server codec
//////////////////////////////////////////

type ServerCodec struct {
	encLock sync.Mutex
	dec     *json.Decoder // for reading JSON values
	enc     *json.Encoder // for writing JSON values
	c       io.Closer

	// temporary work space
	req  serverRequest
	resp serverResponse

	// JSON-RPC clients can use arbitrary json values as request IDs.
	// Package rpc expects uint64 request IDs.
	// We assign uint64 sequence numbers to incoming requests
	// but save the original request ID in the pending map.
	// When rpc responds, we use the sequence number in
	// the response to find the original request ID.
	mutex   sync.Mutex
	seq     int64
	pending map[int64]*json.RawMessage
}

// serverRequest represents a JSON-RPC request received by the server.
type serverRequest struct {
	// JSON-RPC protocol.
	Version string `json:"jsonrpc"`
	// A String containing the name of the method to be invoked.
	Method string `json:"method"`
	// A Structured value to pass as arguments to the method.
	Params *json.RawMessage `json:"params"`
	// The request id. MUST be a string, number or null.
	ID *json.RawMessage `json:"id"`
}

// serverResponse represents a JSON-RPC response returned by the server.
type serverResponse struct {
	// JSON-RPC protocol.
	Version string `json:"jsonrpc"`
	// This must be the same id as the request it is responding to.
	ID *json.RawMessage `json:"id"`
	// The Object that was returned by the invoked method. This must be null
	// in case there was an error invoking the method.
	// As per spec the member will be omitted if there was an error.
	Result interface{} `json:"result,omitempty"`
	// An Error object if there was an error invoking the method. It must be
	// null if there was no error.
	// As per spec the member will be omitted if there was no error.
	Error interface{} `json:"error,omitempty"`
}

var (
	null    = json.RawMessage([]byte("null"))
	Version = "2.0"
	// BatchMethod = "JSONRPC2.Batch"
)

// NewServerCodec returns a new rpc.ServerCodec using JSON-RPC 2.0 on conn,
//
// For most use cases NewServerCodec is too low-level and you should use
// ServeConn instead. You'll need NewServerCodec if you wanna register
// your own object of type named "JSONRPC2" (same as used internally to
// process batch requests) or you wanna use custom rpc server object
// instead of rpc.DefaultServer to process requests on conn.
func NewServerCodec(conn io.ReadWriteCloser) *ServerCodec {
	return &ServerCodec{
		dec:     json.NewDecoder(conn),
		enc:     json.NewEncoder(conn),
		c:       conn,
		pending: make(map[int64]*json.RawMessage),
	}
}

func (r *serverRequest) reset() {
	r.Version = ""
	r.Method = ""
	if r.Params != nil {
		*r.Params = (*r.Params)[:0]
	}
	if r.ID != nil {
		*r.ID = (*r.ID)[:0]
	}
}

func (r *serverRequest) UnmarshalJSON(raw []byte) error {
	r.reset()
	type req *serverRequest
	if err := json.Unmarshal(raw, req(r)); err != nil {
		return jerrors.New("bad request")
	}

	var o = make(map[string]*json.RawMessage)
	if err := json.Unmarshal(raw, &o); err != nil {
		return jerrors.New("bad request")
	}
	if o["jsonrpc"] == nil || o["method"] == nil {
		return jerrors.New("bad request")
	}
	_, okID := o["id"]
	_, okParams := o["params"]
	if len(o) == 3 && !(okID || okParams) || len(o) == 4 && !(okID && okParams) || len(o) > 4 {
		return jerrors.New("bad request")
	}
	if r.Version != Version {
		return jerrors.New("bad request")
	}
	if okParams {
		if r.Params == nil || len(*r.Params) == 0 {
			return jerrors.New("bad request")
		}
		switch []byte(*r.Params)[0] {
		case '[', '{':
		default:
			return jerrors.New("bad request")
		}
	}
	if okID && r.ID == nil {
		r.ID = &null
	}
	if okID {
		if len(*r.ID) == 0 {
			return jerrors.New("bad request")
		}
		switch []byte(*r.ID)[0] {
		case 't', 'f', '{', '[':
			return jerrors.New("bad request")
		}
	}

	return nil
}

func (c *ServerCodec) ReadHeader(m *Message) error {
	// If return error:
	// - codec will be closed
	// So, try to send error reply to client before returning error.
	var raw json.RawMessage
	c.req.reset()
	if err := c.dec.Decode(&raw); err != nil {
		c.encLock.Lock()
		rspError := &Error{Code: -32700, Message: "Parse error"}
		c.enc.Encode(serverResponse{Version: Version, ID: &null, Error: rspError})
		c.encLock.Unlock()
		return err
	}
	if err := json.Unmarshal(raw, &c.req); err != nil {
		if err.Error() == "bad request" {
			c.encLock.Lock()
			rspError := &Error{Code: -32600, Message: "Invalid request"}
			c.enc.Encode(serverResponse{Version: Version, ID: &null, Error: rspError})
			c.encLock.Unlock()
		}
		return err
	}

	m.Method = c.req.Method
	m.Target = m.Header["Path"] // get Path in http_transport.go:Recv:line 242
	if m.Header["HttpMethod"] != "POST" {
		return &Error{Code: -32601, Message: "Method not found"}
	}

	// JSON request id can be any JSON value;
	// RPC package expects uint64.  Translate to
	// internal uint64 and save JSON on the side.
	c.mutex.Lock()
	c.seq++
	c.pending[c.seq] = c.req.ID
	c.req.ID = nil
	m.ID = c.seq
	c.mutex.Unlock()

	return nil
}

// ReadRequest fills the request object for the RPC method.
//
// ReadRequest parses request parameters in two supported forms in
// accordance with http://www.jsonrpc.org/specification#parameter_structures
//
// by-position: params MUST be an Array, containing the
// values in the Server expected order.
//
// by-name: params MUST be an Object, with member names
// that match the Server expected parameter names. The
// absence of expected names MAY result in an error being
// generated. The names MUST match exactly, including
// case, to the method's expected parameters.
func (c *ServerCodec) ReadBody(x interface{}) error {
	// If x!=nil and return error e:
	// - Write() will be called with e.Error() in r.Error
	if x == nil {
		return nil
	}
	if c.req.Params == nil {
		return nil
	}

	var (
		params []byte
		err    error
	)

	// 在这里把请求参数json 字符串转换成了相应的struct
	params = []byte(*c.req.Params)
	if err = json.Unmarshal(*c.req.Params, x); err != nil {
		// Note: if c.request.Params is nil it's not an error, it's an optional member.
		// JSON params structured object. Unmarshal to the args object.

		if 2 < len(params) && params[0] == '[' && params[len(params)-1] == ']' {
			// Clearly JSON params is not a structured object,
			// fallback and attempt an unmarshal with JSON params as
			// array value and RPC params is struct. Unmarshal into
			// array containing the request struct.
			params := [1]interface{}{x}
			if err = json.Unmarshal(*c.req.Params, &params); err != nil {
				return &Error{Code: -32602, Message: "Invalid params, error:" + err.Error()}
			}
		} else {
			return &Error{Code: -32602, Message: "Invalid params, error:" + err.Error()}
		}
	}

	return nil
}

func NewError(code int, message string) *Error {
	return &Error{Code: code, Message: message}
}

func newError(message string) *Error {
	switch {
	case strings.HasPrefix(message, "rpc: service/method request ill-formed"):
		return NewError(-32601, message)
	case strings.HasPrefix(message, "rpc: can't find service"):
		return NewError(-32601, message)
	case strings.HasPrefix(message, "rpc: can't find method"):
		return NewError(-32601, message)
	default:
		return NewError(-32000, message)
	}
}

func (c *ServerCodec) Write(m *Message, x interface{}) error {
	// If return error: nothing happens.
	// In r.Error will be "" or .Error() of error returned by:
	// - ReadBody()
	// - called RPC method
	var (
		err error
	)

	c.mutex.Lock()
	b, ok := c.pending[m.ID]
	if !ok {
		c.mutex.Unlock()
		return jerrors.New("invalid sequence number in response")
	}
	delete(c.pending, m.ID)
	c.mutex.Unlock()

	if b == nil {
		// Notification. Do not respond.
		return nil
	}

	var resp serverResponse = serverResponse{Version: Version, ID: b, Result: x}
	if m.Error == "" {
		if x == nil {
			resp.Result = &null
		} else {
			resp.Result = x
		}
	} else if m.Error[0] == '{' && m.Error[len(m.Error)-1] == '}' {
		// Well& this check for '{'&'}' isn't too strict, but I
		// suppose we're trusting our own RPC methods (this way they
		// can force sending wrong reply or many replies instead
		// of one) and normal errors won't be formatted this way.
		raw := json.RawMessage(m.Error)
		resp.Error = &raw
	} else {
		raw := json.RawMessage(newError(m.Error).Error())
		resp.Error = &raw
	}
	c.encLock.Lock()
	err = c.enc.Encode(resp) // 把resp通过enc关联的conn发送给client
	c.encLock.Unlock()

	return err
}

func (c *ServerCodec) Close() error {
	return c.c.Close()
}
