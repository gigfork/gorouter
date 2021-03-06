package router

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	nats "github.com/cloudfoundry/gonats"
	"net"
	vcap "router/common"
	"router/config"
	"router/proxy"
	"router/util"
	"runtime"
	"time"
)

type Router struct {
	config     *config.Config
	proxy      *Proxy
	natsClient *nats.Client
	registry   *Registry
	varz       Varz
}

func NewRouter(c *config.Config) *Router {
	r := &Router{
		config: c,
	}

	// setup number of procs
	if r.config.GoMaxProcs != 0 {
		runtime.GOMAXPROCS(r.config.GoMaxProcs)
	}

	// setup nats
	r.natsClient = startNATS(r.config.Nats.Host, r.config.Nats.User, r.config.Nats.Pass)

	r.registry = NewRegistry(r.config)
	r.varz = NewVarz(r.registry)
	r.proxy = NewProxy(r.config, r.registry, r.varz)

	varz := &vcap.Varz{
		UniqueVarz: r.varz,
	}

	var host string
	if r.config.Status.Port != 0 {
		host = fmt.Sprintf("%s:%d", r.config.Ip, r.config.Status.Port)
	}

	component := &vcap.VcapComponent{
		Type:        "Router",
		Index:       r.config.Index,
		Host:        host,
		Credentials: []string{r.config.Status.User, r.config.Status.Pass},
		Config:      r.config,
		Logger:      log,
		Varz:        varz,
		Healthz:     "ok",
	}

	vcap.Register(component, r.natsClient)

	return r
}

func (r *Router) SubscribeRegister() {
	s := r.natsClient.NewSubscription("router.register")
	s.Subscribe()

	go func() {
		for m := range s.Inbox {
			var rm registryMessage

			e := json.Unmarshal(m.Payload, &rm)
			if e != nil {
				log.Warnf("Error unmarshalling JSON: %s (%s)", e, string(m.Payload))
				continue
			}

			r.registry.Register(&rm)
		}
	}()
}

func (r *Router) SubscribeUnregister() {
	s := r.natsClient.NewSubscription("router.unregister")
	s.Subscribe()

	go func() {
		for m := range s.Inbox {
			var rm registryMessage

			e := json.Unmarshal(m.Payload, &rm)
			if e != nil {
				log.Warnf("Error unmarshalling JSON: %s (%s)", e, string(m.Payload))
				continue
			}

			r.registry.Unregister(&rm)
		}
	}()
}

func (r *Router) flushApps(t time.Time) {
	x := r.registry.ActiveSince(t)

	y, err := json.Marshal(x)
	if err != nil {
		log.Warnf("json.Marshal: %s", err)
		return
	}

	b := bytes.Buffer{}
	w := zlib.NewWriter(&b)
	w.Write(y)
	w.Close()

	z := b.Bytes()

	log.Debugf("Active apps: %d, message size: %d", len(x), len(z))

	r.natsClient.Publish("router.active_apps", z)
}

func (r *Router) ScheduleFlushApps() {
	if r.config.PublishActiveAppsInterval == 0 {
		return
	}

	go func() {
		t := time.NewTicker(r.config.PublishActiveAppsInterval)
		x := time.Now()

		for {
			select {
			case <-t.C:
				y := time.Now()
				r.flushApps(x)
				x = y
			}
		}
	}()
}

func (r *Router) SendStartMessage() {
	d := map[string]string{"id": vcap.GenerateUUID()}

	b, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}

	// Send start message once at start
	r.natsClient.Publish("router.start", b)

	go func() {
		t := time.NewTicker(r.config.PublishStartMessageInterval)

		for {
			select {
			case <-t.C:
				r.natsClient.Publish("router.start", b)
			}
		}
	}()
}

func (r *Router) Run() {
	var err error

	// Subscribe register/unregister router
	r.SubscribeRegister()
	r.SubscribeUnregister()

	// Kickstart sending start messages
	r.SendStartMessage()

	// Schedule flushing active app's app_id
	r.ScheduleFlushApps()

	// Wait for one start message send interval, such that the router's registry
	// can be populated before serving requests.
	if r.config.PublishStartMessageInterval != 0 {
		log.Infof("Waiting %s before listening...", r.config.PublishStartMessageInterval)
		time.Sleep(r.config.PublishStartMessageInterval)
	}

	l, err := net.Listen("tcp", fmt.Sprintf(":%d", r.config.Port))
	if err != nil {
		log.Fatalf("net.Listen: %s", err)
	}

	util.WritePidFile(r.config.Pidfile)

	log.Infof("Listening on %s", l.Addr())

	s := proxy.Server{Handler: r.proxy}

	err = s.Serve(l)
	if err != nil {
		log.Fatalf("proxy.Serve: %s", err)
	}
}

func startNATS(host, user, pass string) *nats.Client {
	c := nats.NewClient()

	go func() {
		e := c.RunWithDefaults(host, user, pass)
		log.Fatalf("Failed to connect to nats server: %s", e.Error())
	}()

	return c
}
