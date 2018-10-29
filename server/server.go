package server

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	log "github.com/AlexStocks/log4go"
	"github.com/dubbo/dubbo-go/public"
	"github.com/dubbo/dubbo-go/registry"
	jerrors "github.com/juju/errors"
)

// 完成注册任务
type Server struct {
	rpc  []*rpcServer // 处理外部请求,改为数组形式,以监听多个地址
	done chan struct{}
	once sync.Once

	sync.RWMutex
	opts     Options            // codec,transport,registry
	handlers map[string]Handler // interface -> Handler
	wg       sync.WaitGroup
}

func NewServer(opts ...Option) *Server {
	var (
		num int
	)
	options := newOptions(opts...)
	Servers := make([]*rpcServer, len(options.ConfList))
	num = len(options.ConfList)
	for i := 0; i < num; i++ {
		Servers[i] = initServer()
	}
	return &Server{
		opts:     options,
		rpc:      Servers,
		handlers: make(map[string]Handler),
		done:     make(chan struct{}),
	}
}

func (s *Server) handlePkg(servo interface{}, sock *httpTransportSocket) {
	var (
		ok          bool
		rpc         *rpcServer
		pkg         Package
		err         error
		timeout     uint64
		contentType string
		codec       serverCodec
		header      map[string]string
		key         string
		value       string
		ctx         context.Context
	)

	if rpc, ok = servo.(*rpcServer); !ok {
		return
	}

	defer func() { // panic执行之前会保证defer被执行
		if r := recover(); r != nil {
			log.Warn("connection{local:%v, remote:%v} panic error:%#v, debug stack:%s",
				sock.LocalAddr(), sock.RemoteAddr(), r, string(debug.Stack()))
		}

		// close socket
		sock.Close() // 在这里保证了整个逻辑执行完毕后，关闭了连接，回收了socket fd
	}()

	for {
		// 读取请求包
		if err = sock.Recv(&pkg); err != nil {
			return
		}

		// 下面的所有逻辑都是处理请求包，并回复response
		// we use s Content-Type header to identify the codec needed
		contentType = pkg.Header["Content-Type"]

		// codec of jsonrpc
		if contentType != "application/json" && contentType != "application/json-rpc" {
			sock.Send(&Package{
				Header: map[string]string{
					"Content-Type": "text/plain",
				},
				Body: []byte(err.Error()),
			})
			return
		}

		codec = newRPCCodec(&pkg, sock)
		if codec == nil {
			panic(fmt.Sprintf("newRPCCodec(pkg:%#v, sock:%#v) = nil", pkg, sock))
		}

		// strip our headers
		header = make(map[string]string)
		for key, value = range pkg.Header {
			header[key] = value
		}
		delete(header, "Content-Type")
		delete(header, "Timeout")

		ctx = context.WithValue(context.Background(), public.DUBBOGO_CTX_KEY, header)
		// we use s Timeout header to set a Server deadline
		if len(pkg.Header["Timeout"]) > 0 {
			if timeout, err = strconv.ParseUint(pkg.Header["Timeout"], 10, 64); err == nil {
				ctx, _ = context.WithTimeout(ctx, time.Duration(timeout))
			}
		}

		if err = rpc.serveRequest(ctx, codec); err != nil {
			log.Info("Unexpected error serving request, closing socket: %v", err)
			return
		}
	}
}

func (s *Server) Options() Options {
	var (
		opts Options
	)

	s.RLock()
	opts = s.opts
	s.RUnlock()

	return opts
}

/*
type ProviderServiceConfig struct {
	Protocol string // from ServiceConfig, get field{Path} from ServerConfig by s field
	Service string  // from handler, get field{Protocol, Group, Version} from ServiceConfig by s field
	Group   string
	Version string
	Methods string
	Path    string
}

type ServiceConfig struct {
	Protocol string `required:"true",default:"dubbo"` // codec string, jsonrpc etc
	Service string `required:"true"`
	Group   string
	Version string
}

type ServerConfig struct {
	Protocol string `required:"true",default:"dubbo"` // codec string, jsonrpc etc
	IP       string
	Port     int
}
*/

func (s *Server) Handle(h Handler) error {
	var (
		i           int
		j           int
		flag        int
		serviceNum  int
		ServerNum   int
		err         error
		config      Options
		serviceConf registry.ProviderServiceConfig
	)
	config = s.Options()

	serviceConf.Service = h.Service()
	serviceConf.Version = h.Version()

	flag = 0
	serviceNum = len(config.ServiceConfList)
	ServerNum = len(config.ConfList)
	for i = 0; i < serviceNum; i++ {
		if config.ServiceConfList[i].Service == serviceConf.Service &&
			config.ServiceConfList[i].Version == serviceConf.Version {

			serviceConf.Protocol = config.ServiceConfList[i].Protocol
			serviceConf.Group = config.ServiceConfList[i].Group
			// serviceConf.Version = config.ServiceConfList[i].Version
			for j = 0; j < ServerNum; j++ {
				if config.ConfList[j].Protocol == serviceConf.Protocol {
					s.Lock()
					serviceConf.Methods, err = s.rpc[j].register(h)
					s.Unlock()
					if err != nil {
						return err
					}

					serviceConf.Path = config.ConfList[j].Address()
					err = config.Registry.Register(serviceConf)
					if err != nil {
						return err
					}
					flag = 1
				}
			}
		}
	}

	if flag == 0 {
		return jerrors.Errorf("fail to register Handler{service:%s, version:%s}", serviceConf.Service, serviceConf.Version)
	}

	s.Lock()
	s.handlers[h.Service()] = h
	s.Unlock()

	return nil
}

func accept(listener net.Listener, fn func(*httpTransportSocket)) error {
	var (
		err      error
		c        net.Conn
		ok       bool
		ne       net.Error
		tmpDelay time.Duration
	)

	for {
		c, err = listener.Accept()
		if err != nil {
			if ne, ok = err.(net.Error); ok && ne.Temporary() {
				if tmpDelay != 0 {
					tmpDelay <<= 1
				} else {
					tmpDelay = 5 * time.Millisecond
				}
				if tmpDelay > DefaultMaxSleepTime {
					tmpDelay = DefaultMaxSleepTime
				}
				log.Info("http: Accept error: %v; retrying in %v\n", err, tmpDelay)
				time.Sleep(tmpDelay)
				continue
			}
			return jerrors.Trace(err)
		}

		go func() {
			defer func() {
				if r := recover(); r != nil {
					const size = 64 << 10
					buf := make([]byte, size)
					buf = buf[:runtime.Stack(buf, false)]
					log.Error("http: panic serving %v: %v\n%s", c.RemoteAddr(), r, buf)
					listener.Close()
				}
			}()

			fn(InitHTTPTransportSocket(c))
		}()
	}
}

func (s *Server) Start() error {
	var (
		i         int
		ServerNum int
		err       error
		config    Options
		rpc       *rpcServer
		listener  net.Listener
	)

	config = s.Options()

	ServerNum = len(config.ConfList)
	for i = 0; i < ServerNum; i++ {
		// listener, err = tr.Listen(config.ConfList[i].Address())
		listener, err = net.Listen("tcp", config.ConfList[i].Address())
		if err != nil {
			return err
		}
		log.Info("Listening on %s", listener.Addr())

		s.Lock()
		rpc = s.rpc[i]
		// rpc.listener = listener
		s.Unlock()

		s.wg.Add(1)
		go func(servo *rpcServer) {
			accept(listener, func(sock *httpTransportSocket) { s.handlePkg(rpc, sock) })
			s.wg.Done()
		}(rpc)

		s.wg.Add(1)
		go func(servo *rpcServer) { // Server done goroutine
			var err error
			<-s.done               // step1: block to wait for done channel(wait Server.Stop step2)
			err = listener.Close() // step2: and then close listener
			if err != nil {
				log.Warn("listener{addr:%s}.Close() = error{%#v}", listener.Addr(), err)
			}
			s.wg.Done()
		}(rpc)
	}

	return nil
}

func (s *Server) Stop() {
	s.once.Do(func() {
		close(s.done)
		s.wg.Wait()
		if s.opts.Registry != nil {
			s.opts.Registry.Close()
			s.opts.Registry = nil
		}
	})
}

func (s *Server) String() string {
	return "dubbogo-Server"
}
