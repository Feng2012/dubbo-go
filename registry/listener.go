package registry

import (
	"fmt"
	"path"
	"sync"
	"time"
)

import (
	log "github.com/AlexStocks/log4go"
	jerrors "github.com/juju/errors"
	"github.com/samuel/go-zookeeper/zk"
)

const (
	MAX_TIMES                   = 15
	ZkEvent_Channel_Size        = 32
	ZKCLIENT_EVENT_CHANNEL_SIZE = 4
)

type zkEvent struct {
	res *ServiceURLEvent
	err error
}

func (e zkEvent) String() string {
	return fmt.Sprintf("err:%s, res:%s", e.err, e.res)
}

type zkEventListener struct {
	client         *zookeeperClient
	events         chan zkEvent
	serviceMapLock sync.Mutex
	serviceMap     map[string]struct{}
	wg             sync.WaitGroup
}

func newZkEventListener(client *zookeeperClient) *zkEventListener {
	return &zkEventListener{
		client:     client,
		events:     make(chan zkEvent, ZkEvent_Channel_Size),
		serviceMap: make(map[string]struct{}),
	}
}

func (l *zkEventListener) listenServiceNodeEvent(zkPath string) bool {
	l.wg.Add(1)
	defer l.wg.Done()
	var zkEvent zk.Event
	for {
		keyEventCh, err := l.client.existW(zkPath)
		if err != nil {
			log.Error("existW{key:%s} = error{%v}", zkPath, err)
			return false
		}

		select {
		case zkEvent = <-keyEventCh:
			log.Warn("get a zookeeper zkEvent{type:%s, server:%s, path:%s, state:%d-%s, err:%s}",
				zkEvent.Type.String(), zkEvent.Server, zkEvent.Path, zkEvent.State, stateToString(zkEvent.State), zkEvent.Err)
			switch zkEvent.Type {
			case zk.EventNodeDataChanged:
				log.Warn("zk.ExistW(key{%s}) = event{EventNodeDataChanged}", zkPath)
			case zk.EventNodeCreated:
				log.Warn("zk.ExistW(key{%s}) = event{EventNodeCreated}", zkPath)
			case zk.EventNotWatching:
				log.Warn("zk.ExistW(key{%s}) = event{EventNotWatching}", zkPath)
			case zk.EventNodeDeleted:
				log.Warn("zk.ExistW(key{%s}) = event{EventNodeDeleted}", zkPath)
				return true
			}
		case <-l.client.done():
			return false
		}
	}

	return false
}

func (l *zkEventListener) handleZkNodeEvent(zkPath string, children []string, conf ServiceConfig) {
	contains := func(s []string, e string) bool {
		for _, a := range s {
			if a == e {
				return true
			}
		}

		return false
	}

	newChildren, err := l.client.getChildren(zkPath)
	if err != nil {
		log.Error("path{%s} child nodes changed, zk.Children() = error{%v}", zkPath, jerrors.ErrorStack(err))
		return
	}

	// a node was added -- listen the new node
	var (
		newNode    string
		serviceURL *ServiceURL
	)
	for _, n := range newChildren {
		if contains(children, n) {
			continue
		}

		newNode = path.Join(zkPath, n)
		log.Info("add zkNode{%s}", newNode)
		serviceURL, err = NewServiceURL(n)
		if err != nil {
			log.Error("NewServiceURL(%s) = error{%v}", n, jerrors.ErrorStack(err))
			continue
		}
		if !conf.ServiceEqual(serviceURL) {
			log.Warn("serviceURL{%s} is not compatible with ServiceConfig{%#v}", serviceURL, conf)
			continue
		}
		log.Info("add serviceURL{%s}", serviceURL)
		l.events <- zkEvent{&ServiceURLEvent{ServiceURLAdd, serviceURL}, nil}
		// listen l service node
		go func(node string, serviceURL *ServiceURL) {
			log.Info("delete zkNode{%s}", node)
			if l.listenServiceNodeEvent(node) {
				log.Info("delete serviceURL{%s}", serviceURL)
				l.events <- zkEvent{&ServiceURLEvent{ServiceURLDel, serviceURL}, nil}
			}
			log.Warn("listenSelf(zk path{%s}) goroutine exit now", zkPath)
		}(newNode, serviceURL)
	}

	// old node was deleted
	var oldNode string
	for _, n := range children {
		if contains(newChildren, n) {
			continue
		}

		oldNode = path.Join(zkPath, n)
		log.Warn("delete zkPath{%s}", oldNode)
		serviceURL, err = NewServiceURL(n)
		if !conf.ServiceEqual(serviceURL) {
			log.Warn("serviceURL{%s} has been deleted is not compatible with ServiceConfig{%#v}", serviceURL, conf)
			continue
		}
		log.Warn("delete serviceURL{%s}", serviceURL)
		if err != nil {
			log.Error("NewServiceURL(i{%s}) = error{%v}", n, jerrors.ErrorStack(err))
			continue
		}
		l.events <- zkEvent{&ServiceURLEvent{ServiceURLDel, serviceURL}, nil}
	}
}

func (l *zkEventListener) listenDirEvent(zkPath string, conf ServiceConfig) {
	l.wg.Add(1)
	defer l.wg.Done()

	var (
		failTimes int
		event     chan struct{}
		zkEvent   zk.Event
	)
	event = make(chan struct{}, ZKCLIENT_EVENT_CHANNEL_SIZE)
	defer close(event)
	for {
		// get current children for a zkPath
		children, childEventCh, err := l.client.getChildrenW(zkPath)
		if err != nil {
			failTimes++
			if MAX_TIMES <= failTimes {
				failTimes = MAX_TIMES
			}
			log.Error("listenDirEvent(path{%s}) = error{%v}", zkPath, err)
			// clear the event channel
		CLEAR:
			for {
				select {
				case <-event:
				default:
					break CLEAR
				}
			}
			l.client.registerEvent(zkPath, &event)
			select {
			case <-time.After(timeSecondDuration(failTimes * REGISTRY_CONN_DELAY)):
				l.client.unregisterEvent(zkPath, &event)
				continue
			case <-l.client.done():
				l.client.unregisterEvent(zkPath, &event)
				log.Warn("client.done(), listen(path{%s}, ServiceConfig{%#v}) goroutine exit now...", zkPath, conf)
				return
			case <-event:
				log.Info("get zk.EventNodeDataChange notify event")
				l.client.unregisterEvent(zkPath, &event)
				l.handleZkNodeEvent(zkPath, nil, conf)
				continue
			}
		}
		failTimes = 0

		select {
		case zkEvent = <-childEventCh:
			log.Warn("get a zookeeper zkEvent{type:%s, server:%s, path:%s, state:%d-%s, err:%s}",
				zkEvent.Type.String(), zkEvent.Server, zkEvent.Path, zkEvent.State, stateToString(zkEvent.State), zkEvent.Err)
			if zkEvent.Type != zk.EventNodeChildrenChanged {
				continue
			}
			l.handleZkNodeEvent(zkEvent.Path, children, conf)
		case <-l.client.done():
			log.Warn("client.done(), listen(path{%s}, ServiceConfig{%#v}) goroutine exit now...", zkPath, conf)
			return
		}
	}
}

// this func is invoked by ZkConsumerRegistry::Registe/ZkConsumerRegistry::get/ZkConsumerRegistry::getListener
// registry.go:Listen -> listenServiceEvent -> listenDirEvent -> listenServiceNodeEvent
//                            |
//                            --------> listenServiceNodeEvent
func (l *zkEventListener) listenServiceEvent(conf ServiceConfig) {
	var (
		err        error
		zkPath     string
		dubboPath  string
		children   []string
		serviceURL *ServiceURL
	)

	zkPath = fmt.Sprintf("/dubbo/%s/providers", conf.Service)

	l.serviceMapLock.Lock()
	_, ok := l.serviceMap[zkPath]
	l.serviceMapLock.Unlock()
	if ok {
		log.Warn("@zkPath %s has already been listened.", zkPath)
		return
	}

	l.serviceMapLock.Lock()
	l.serviceMap[zkPath] = struct{}{}
	l.serviceMapLock.Unlock()

	log.Info("listen dubbo provider path{%s} event and wait to get all provider zk nodes", zkPath)
	children, err = l.client.getChildren(zkPath)
	if err != nil {
		children = nil
		log.Error("fail to get children of zk path{%s}", zkPath)
	}

	for _, c := range children {
		serviceURL, err = NewServiceURL(c)
		if err != nil {
			log.Error("NewServiceURL(r{%s}) = error{%v}", c, err)
			continue
		}
		if !conf.ServiceEqual(serviceURL) {
			log.Warn("serviceURL{%s} is not compatible with ServiceConfig{%#v}", serviceURL, conf)
			continue
		}
		log.Debug("add serviceUrl{%s}", serviceURL)
		l.events <- zkEvent{&ServiceURLEvent{ServiceURLAdd, serviceURL}, nil}

		// listen l service node
		dubboPath = path.Join(zkPath, c)
		log.Info("listen dubbo service key{%s}", dubboPath)
		go func(zkPath string, serviceURL *ServiceURL) {
			if l.listenServiceNodeEvent(dubboPath) {
				log.Debug("delete serviceUrl{%s}", serviceURL)
				l.events <- zkEvent{&ServiceURLEvent{ServiceURLDel, serviceURL}, nil}
			}
			log.Warn("listenSelf(zk path{%s}) goroutine exit now", zkPath)
		}(dubboPath, serviceURL)
	}

	log.Info("listen dubbo path{%s}", zkPath)
	go func(zkPath string, conf ServiceConfig) {
		l.listenDirEvent(zkPath, conf)
		log.Warn("listenDirEvent(zkPath{%s}) goroutine exit now", zkPath)
	}(zkPath, conf)
}

func (l *zkEventListener) listenEvent(r *ZkConsumerRegistry) error {
	for {
		select {
		case <-l.client.done():
			log.Warn("listener's zk client connection is broken, so zk event listener exit now.")
			l.close()
			return jerrors.New("listener stopped")

		case <-r.done:
			log.Warn("zk consumer register has quit, so zk event listener exit asap now.")
			l.close()
			return jerrors.New("listener stopped")

		case e := <-l.events:
			log.Debug("got zk event %s", e)
			if e.err != nil {
				return jerrors.Trace(e.err)
			}
			if e.res.Action == ServiceURLDel && !l.valid() {
				log.Warn("update @result{%s}. But its connection to registry is invalid", e.res)
				continue
			}
			r.update(e.res)
		}
	}
}

func (l *zkEventListener) valid() bool {
	return l.client.zkConnValid()
}

func (l *zkEventListener) close() {
	l.client.Close()
	l.wg.Wait()
}
