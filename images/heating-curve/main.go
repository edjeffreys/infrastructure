package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	statestreamPrefix = "homeassistant.statestream."
	sampleSubject     = "heating.curve.sample"
	// Covers the 1h means plus the 3h change filters, with room to spare.
	lookback = 6 * time.Hour
	// Lets statestream messages for the hour arrive before it is evaluated.
	grace = 30 * time.Second
)

type config struct {
	url, ca, cert, key string
	energyStream       string
	samplesStream      string
	backfill           time.Duration
	listen             string
	fit                fitConfig
}

func main() {
	var c config
	flag.StringVar(&c.url, "nats-url", "tls://nats.nats.svc:4222", "NATS server")
	flag.StringVar(&c.ca, "nats-ca", "/etc/nats-ca/ca.crt", "CA bundle")
	flag.StringVar(&c.cert, "nats-cert", "/etc/nats-client/tls.crt", "client certificate")
	flag.StringVar(&c.key, "nats-key", "/etc/nats-client/tls.key", "client key")
	flag.StringVar(&c.energyStream, "energy-stream", "ENERGY", "stream holding HA statestream")
	flag.StringVar(&c.samplesStream, "samples-stream", "HEATING", "stream holding hourly samples")
	flag.DurationVar(&c.backfill, "backfill", 7*24*time.Hour, "hours to compute from history on first start")
	flag.StringVar(&c.listen, "listen", ":8080", "health endpoint")
	flag.DurationVar(&c.fit.HalfLife, "half-life", 30*24*time.Hour, "sample weight half-life")
	flag.Float64Var(&c.fit.BinWidth, "bin-width", 2, "outdoor temperature bin width, °C")
	flag.Float64Var(&c.fit.MinN, "min-samples", 24, "weighted samples needed before recommending")
	flag.BoolVar(&c.fit.ExcludeDHW, "exclude-dhw", false, "leave hours with hot-water use out of the fit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, c); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func connect(c config) (*nats.Conn, error) {
	pem, err := os.ReadFile(c.ca)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates in %s", c.ca)
	}
	cert, err := tls.LoadX509KeyPair(c.cert, c.key)
	if err != nil {
		return nil, err
	}
	return nats.Connect(c.url,
		nats.Name("heating-curve"),
		nats.Secure(&tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { slog.Warn("nats disconnected", "err", err) }),
		nats.ReconnectHandler(func(*nats.Conn) { slog.Info("nats reconnected") }),
	)
}

type observation struct {
	entity string
	t      time.Time
	raw    string
	caught bool
}

type engine struct {
	cfg     config
	nc      *nats.Conn
	js      jetstream.JetStream
	hist    history
	samples []sample
	next    time.Time
	caught  bool
}

func run(ctx context.Context, c config) error {
	nc, err := connect(c)
	if err != nil {
		return err
	}
	defer nc.Drain()
	go serveHealth(c.listen, nc)
	return runWith(ctx, c, nc)
}

func runWith(ctx context.Context, c config, nc *nats.Conn) error {
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}

	e := &engine{cfg: c, nc: nc, js: js, hist: history{}}
	if err := e.loadSamples(ctx); err != nil {
		return fmt.Errorf("loading samples: %w", err)
	}
	now := time.Now()
	if n := len(e.samples); n > 0 {
		e.next = e.samples[n-1].Hour.Add(time.Hour)
	} else {
		e.next = now.Add(-c.backfill).Truncate(time.Hour)
	}
	slog.Info("starting", "samples", len(e.samples), "next_hour", e.next)

	energy, err := js.Stream(ctx, c.energyStream)
	if err != nil {
		return fmt.Errorf("stream %s: %w", c.energyStream, err)
	}
	start := e.next.Add(-time.Hour - lookback)
	if err := e.seed(ctx, energy, start); err != nil {
		return err
	}

	obs := make(chan observation, 1024)
	cons, err := energy.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{statestreamPrefix + ">"},
		DeliverPolicy:  jetstream.DeliverByStartTimePolicy,
		OptStartTime:   &start,
	})
	if err != nil {
		return fmt.Errorf("consumer on %s: %w", c.energyStream, err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		md, err := m.Metadata()
		if err != nil {
			return
		}
		// Untracked entities still go through: the last message may carry NumPending == 0.
		ent, _ := entityOf(m.Subject())
		select {
		case obs <- observation{ent, md.Timestamp, string(m.Data()), md.NumPending == 0}:
		case <-ctx.Done():
		}
	})
	if err != nil {
		return err
	}
	defer cc.Stop()

	haOnline := make(chan struct{}, 1)
	sub, err := nc.Subscribe(haStatusSubject, func(m *nats.Msg) {
		if strings.TrimSpace(string(m.Data)) == "online" {
			select {
			case haOnline <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	started := time.Now()
	lastObs := started
	var republish <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case o := <-obs:
			lastObs = time.Now()
			if !e.caught && e.next.Before(o.t) {
				e.advance(ctx, o.t)
			}
			if tracked[o.entity] {
				e.hist.get(o.entity).add(o.t, o.raw)
			}
			if !e.caught && o.caught {
				e.catchUp(ctx)
			}
		case <-tick.C:
			// A replay window with nothing in it never delivers a NumPending == 0.
			if !e.caught && time.Since(lastObs) > 10*time.Second {
				e.catchUp(ctx)
			}
			if e.caught {
				e.advance(ctx, time.Now().Add(-grace))
			}
		case <-haOnline:
			if e.caught {
				e.publishDiscovery()
				// HA subscribes to a state topic only after processing its config.
				republish = time.After(10 * time.Second)
			}
		case <-republish:
			e.publishState()
		}
	}
}

func entityOf(subj string) (string, bool) {
	rest, ok := strings.CutPrefix(subj, statestreamPrefix)
	if !ok {
		return "", false
	}
	rest, ok = strings.CutSuffix(rest, ".state")
	if !ok {
		return "", false
	}
	domain, obj, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(obj, ".") {
		return "", false
	}
	return domain + "." + obj, true
}

// seed loads each entity's last state from before start. Curve settings can
// sit unchanged for weeks, far beyond any replay window.
func (e *engine) seed(ctx context.Context, energy jetstream.Stream, start time.Time) error {
	for _, ent := range entities {
		domain, obj, _ := strings.Cut(ent, ".")
		m, err := energy.GetLastMsgForSubject(ctx, statestreamPrefix+domain+"."+obj+".state")
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			slog.Warn("no state in stream yet", "entity", ent)
			continue
		}
		if err != nil {
			return fmt.Errorf("last state of %s: %w", ent, err)
		}
		if m.Time.Before(start) {
			e.hist.get(ent).add(m.Time, string(m.Data))
		}
	}
	return nil
}

func (e *engine) loadSamples(ctx context.Context) error {
	s, err := e.js.Stream(ctx, e.cfg.samplesStream)
	if err != nil {
		return err
	}
	info, err := s.Info(ctx)
	if err != nil {
		return err
	}
	if info.State.Msgs == 0 {
		return nil
	}
	cons, err := s.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{FilterSubjects: []string{sampleSubject}})
	if err != nil {
		return err
	}
	for {
		m, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			return err
		}
		var smp sample
		if err := json.Unmarshal(m.Data(), &smp); err != nil {
			slog.Warn("skipping unreadable sample", "err", err)
		} else if n := len(e.samples); n == 0 || smp.Hour.After(e.samples[n-1].Hour) {
			e.samples = append(e.samples, smp)
		}
		md, err := m.Metadata()
		if err != nil {
			return err
		}
		if md.NumPending == 0 {
			return nil
		}
	}
}

func (e *engine) catchUp(ctx context.Context) {
	e.caught = true
	e.advance(ctx, time.Now().Add(-grace))
	slog.Info("caught up", "samples", len(e.samples))
	e.publishDiscovery()
	e.publishState()
}

// advance evaluates every hour boundary up to and including to.
func (e *engine) advance(ctx context.Context, to time.Time) {
	evaluated := false
	for !e.next.After(to) {
		s := e.hist.evaluate(e.next)
		e.next = e.next.Add(time.Hour)
		b, _ := json.Marshal(s)
		hdr := nats.Header{}
		hdr.Set(jetstream.MsgIDHeader, "sample-"+s.Hour.UTC().Format(time.RFC3339))
		if _, err := e.js.PublishMsg(ctx, &nats.Msg{Subject: sampleSubject, Data: b, Header: hdr}); err != nil {
			slog.Error("storing sample", "hour", s.Hour, "err", err)
		}
		e.samples = append(e.samples, s)
		evaluated = true
		if e.caught {
			slog.Info("sample", "hour", s.Hour, "reason", s.Reason, "required_flow", s.RequiredFlow, "outdoor", s.Outdoor)
		}
	}
	if evaluated {
		e.hist.prune(e.next.Add(-time.Hour - lookback))
		if e.caught {
			e.publishState()
		}
	}
}

func (e *engine) publishDiscovery() {
	for subj, b := range haDiscovery() {
		if err := e.nc.Publish(subj, b); err != nil {
			slog.Error("publishing discovery", "subject", subj, "err", err)
		}
	}
}

func (e *engine) publishState() {
	now := time.Now()
	slope, _ := e.hist.get(entSlope).numberAt(now)
	shift, _ := e.hist.get(entShift).numberAt(now)
	// The target, not the active setpoint, which solar compensation and the cutoff move.
	setpoint, ok := e.hist.get(entTarget).numberAt(now)
	if !ok {
		setpoint, _ = e.hist.get(entSetpoint).numberAt(now)
	}
	r := fit(e.samples, now, slope, e.cfg.fit)
	var last *sample
	if n := len(e.samples); n > 0 {
		last = &e.samples[n-1]
	}
	state, attrs := haPayloads(r, last, slope, shift, setpoint, now)
	if err := e.nc.Publish(subject(haAttrsTopic), attrs); err != nil {
		slog.Error("publishing attributes", "err", err)
	}
	if err := e.nc.Publish(subject(haStateTopic), state); err != nil {
		slog.Error("publishing state", "err", err)
	}
}

func serveHealth(addr string, nc *nats.Conn) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !nc.IsConnected() {
			http.Error(w, nc.Status().String(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("health endpoint", "err", err)
		os.Exit(1)
	}
}
