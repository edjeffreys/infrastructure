package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestParseHistory(t *testing.T) {
	body := `[[{"entity_id":"sensor.house_temperature","state":"18.5","last_changed":"2026-11-14T00:00:00+00:00"},
	           {"entity_id":"sensor.house_temperature","state":"unavailable","last_changed":"2026-11-14T00:30:00+00:00"}],
	          [{"entity_id":"sensor.not_tracked","state":"1","last_changed":"2026-11-14T00:00:00+00:00"}]]`
	h, err := parseHistory(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := h.get(entHouse).numberAt(at(10)); got != 18.5 {
		t.Fatalf("got %v", got)
	}
	if _, ok := h.get(entHouse).numberAt(at(40)); ok {
		t.Fatal("unavailable should read as missing")
	}
	if _, ok := h["sensor.not_tracked"]; ok {
		t.Fatal("untracked entity kept")
	}
}

func TestLoadSamplesLaterMessageWins(t *testing.T) {
	nc := startServer(t)
	ctx := context.Background()
	js, _ := jetstream.New(nc)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "HEATING", Subjects: []string{"heating.curve.>"}}); err != nil {
		t.Fatal(err)
	}
	pub := func(s sample) {
		b, _ := json.Marshal(s)
		if _, err := js.Publish(ctx, sampleSubject, b); err != nil {
			t.Fatal(err)
		}
	}
	h1, h2 := at(60), at(120)
	pub(sample{Hour: h1, Reason: "missing data"})
	pub(sample{Hour: h2, Reason: "missing data"})
	pub(sample{Hour: h1, Valid: true, Reason: "ok"})

	e := &engine{cfg: config{samplesStream: "HEATING"}, js: js}
	if err := e.loadSamples(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.samples) != 2 || !e.samples[0].Hour.Equal(h1) || !e.samples[0].Valid || !e.samples[1].Hour.Equal(h2) {
		t.Fatalf("got %+v", e.samples)
	}
}

func TestImportHA(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		start := time.Now().Add(-30 * time.Hour).UTC().Format(time.RFC3339)
		var lists [][]haState
		for e, v := range map[string]string{
			entHouse: "19", entOutdoor: "0", entSupply: "45", entDHWGas: "1", entSetpoint: "19",
			entSlope: "1.6", entShift: "-6", entClimate: "auto", entSolarGain: "off", entTarget: "19",
		} {
			ts, _ := time.Parse(time.RFC3339, start)
			lists = append(lists, []haState{{EntityID: e, State: v, LastChanged: ts}})
		}
		json.NewEncoder(w).Encode(lists)
	}))
	defer srv.Close()
	t.Setenv("HA_TOKEN", "secret")

	var out bytes.Buffer
	if err := importHA([]string{"-ha-url", srv.URL, "-days", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" || !strings.HasPrefix(gotPath, "/api/history/period/") {
		t.Fatalf("auth %q path %q", gotAuth, gotPath)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 24 || len(lines) > 25 {
		t.Fatalf("want a day of hourly samples, got %d", len(lines))
	}
	var s sample
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &s); err != nil || !s.Valid {
		t.Fatalf("last sample %+v %v", s, err)
	}
}
