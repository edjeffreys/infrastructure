package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func startServer(t *testing.T) *nats.Conn {
	t.Helper()
	s, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func TestEndToEnd(t *testing.T) {
	nc := startServer(t)
	ctx := context.Background()
	js, _ := jetstream.New(nc)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ENERGY", Subjects: []string{"homeassistant.statestream.>"}}); err != nil {
		t.Fatal(err)
	}
	heating, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "HEATING", Subjects: []string{"heating.curve.>"}})
	if err != nil {
		t.Fatal(err)
	}
	for ent, v := range map[string]string{
		"sensor.house_temperature": "18.4", "number.boiler_heating_curve_slope": "1.6", "climate.boiler_heating": "auto",
	} {
		if _, err := js.Publish(ctx, statestreamPrefix+ent+".state", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}

	configs := make(chan string, 64)
	states := make(chan map[string]any, 64)
	nc.Subscribe("homeassistant.sensor.heating_curve_advisor.*.config", func(m *nats.Msg) { configs <- m.Subject })
	nc.Subscribe("homeassistant.heating_curve.state", func(m *nats.Msg) {
		var st map[string]any
		json.Unmarshal(m.Data, &st)
		states <- st
	})

	cfg := config{energyStream: "ENERGY", samplesStream: "HEATING", backfill: 3 * time.Hour, fit: testFit}
	runFor := func() (stop func()) {
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- runWith(rctx, cfg, nc) }()
		return func() {
			cancel()
			if err := <-done; err != context.Canceled {
				t.Fatalf("run: %v", err)
			}
		}
	}

	stop := runFor()
	seen := map[string]bool{}
	for len(seen) < len(haSensors) {
		seen[waitFor(t, configs, "discovery")] = true
	}
	st := waitFor(t, states, "state")
	if st["sample_reason"] != "missing data" {
		t.Errorf("state %v", st)
	}

	info, _ := heating.Info(ctx)
	if n := info.State.Msgs; n < 3 || n > 4 {
		t.Fatalf("want 3 or 4 hourly samples from a 3h backfill, got %d", n)
	}
	before := info.State.Msgs

	nc.Publish(haStatusSubject, []byte("online"))
	waitFor(t, configs, "discovery after HA restart")
	stop()

	stop = runFor()
	waitFor(t, states, "state after restart")
	stop()
	e := &engine{cfg: cfg, js: js}
	if err := e.loadSamples(ctx); err != nil {
		t.Fatal(err)
	}
	info, _ = heating.Info(ctx)
	if uint64(len(e.samples)) != info.State.Msgs || info.State.Msgs > before+1 {
		t.Fatalf("restart re-stored samples: %d before, %d after, %d distinct hours", before, info.State.Msgs, len(e.samples))
	}
}
