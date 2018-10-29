package server

import (
	"context"
	"time"
)

import (
	"github.com/AlexStocks/goext/net"
	"github.com/dubbo/dubbo-go/registry"
)

type Options struct {
	Registry        registry.Registry
	ConfList        []registry.ServerConfig
	ServiceConfList []registry.ServiceConfig
	Timeout         time.Duration
	// Other options for implementations of the interface
	// can be stored in a context
	Context context.Context
}

func newOptions(opt ...Option) Options {
	opts := Options{}
	for _, o := range opt {
		o(&opts)
	}

	if opts.Registry == nil {
		panic("server.Options.Registry is nil")
	}

	return opts
}

// Registry used for discovery
func Registry(r registry.Registry) Option {
	return func(o *Options) {
		o.Registry = r
	}
}

func ConfList(confList []registry.ServerConfig) Option {
	return func(o *Options) {
		o.ConfList = confList
		for i := 0; i < len(o.ConfList); i++ {
			if o.ConfList[i].IP == "" {
				o.ConfList[i].IP, _ = gxnet.GetLocalIP()
			}
		}
	}
}

func ServiceConfList(confList []registry.ServiceConfig) Option {
	return func(o *Options) {
		o.ServiceConfList = confList
	}
}
