package main

import (
	"math"
	"strconv"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func TestMeanIsTimeWeighted(t *testing.T) {
	var s series
	s.add(at(-30), "10")
	s.add(at(45), "20")
	for i := 0; i < 10; i++ {
		s.add(at(50+i), "20")
	}
	got, ok := s.mean(at(0), at(60))
	if !ok || math.Abs(got-12.5) > 1e-9 {
		t.Fatalf("got %v %v, want 12.5", got, ok)
	}
}

func TestMeanNeedsHalfTheWindow(t *testing.T) {
	var s series
	s.add(at(-30), "10")
	s.add(at(20), "unavailable")
	if _, ok := s.mean(at(0), at(60)); ok {
		t.Fatal("20 of 60 minutes covered should not be enough")
	}
}

func TestRiseIgnoresMidnightReset(t *testing.T) {
	var s series
	s.add(at(-10), "3.2")
	s.add(at(10), "0")
	if got := s.rise(at(0), at(60)); got != 0 {
		t.Fatalf("reset read as use: %v", got)
	}
	s.add(at(30), "0.4")
	if got := s.rise(at(0), at(60)); math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("got %v", got)
	}
}

func TestLastChangeIgnoresRepublishAndBlips(t *testing.T) {
	var s series
	s.add(at(-600), "1.6")
	s.add(at(-60), "unavailable")
	s.add(at(-59), "1.6")
	got, ok := s.lastChange(at(0))
	if !ok || !got.Equal(at(-600)) {
		t.Fatalf("got %v", got)
	}
	s.add(at(-30), "1.5")
	if got, _ := s.lastChange(at(0)); !got.Equal(at(-30)) {
		t.Fatalf("got %v", got)
	}
}

func TestPruneKeepsValueInForce(t *testing.T) {
	var s series
	s.add(at(-600), "a")
	s.add(at(-500), "b")
	s.add(at(-10), "c")
	s.prune(at(-60))
	if v, _ := s.at(at(-30)); v != "b" || len(s.points) != 2 {
		t.Fatalf("got %v %v", v, s.points)
	}
}

func steadyHouse(indoor, outdoor float64) history {
	h := history{}
	start := at(-24 * 60)
	h.get(entHouse).add(start, ftoa(indoor))
	h.get(entOutdoor).add(start, ftoa(outdoor))
	h.get(entSupply).add(start, "45")
	h.get(entDHWGas).add(start, "1.0")
	h.get(entSetpoint).add(start, "19")
	h.get(entSlope).add(start, "1.6")
	h.get(entShift).add(start, "-6")
	h.get(entClimate).add(start, "auto")
	h.get(entSolarGain).add(start, "off")
	return h
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func TestEvaluateAtSetpointNeedsTheCurve(t *testing.T) {
	s := steadyHouse(19, 0).evaluate(t0)
	if !s.Valid || s.RequiredFlow != round(curveFlow(1.6, -6, 0, 19), 1) {
		t.Fatalf("got %+v", s)
	}
}

func TestEvaluateColdHouseNeedsMore(t *testing.T) {
	s := steadyHouse(18, 0).evaluate(t0)
	if !s.Valid || s.RequiredFlow <= s.CurveFlow {
		t.Fatalf("got %+v", s)
	}
	// Same arithmetic as the HA advisor.
	mwt := curveFlow(1.6, -6, 0, 19) - 5
	want := 19 + (mwt-18)*math.Pow(19.0/18.0, 1/1.3) + 5
	if math.Abs(s.RequiredFlow-round(want, 1)) > 1e-9 {
		t.Fatalf("got %v, want %v", s.RequiredFlow, round(want, 1))
	}
}

func TestEvaluateReasons(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(h history)
		want   string
	}{
		{"missing", func(h history) { h.get(entHouse).add(at(-59), "unavailable") }, "missing data"},
		{"off", func(h history) { h.get(entClimate).add(at(-5), "off") }, "heating off"},
		{"solar", func(h history) { h.get(entSolarGain).add(at(-5), "on") }, "solar gain"},
		{"mild", func(h history) { h.get(entOutdoor).add(at(-120), "13") }, "too mild: boiler at minimum output"},
		{"setpoint", func(h history) { h.get(entSetpoint).add(at(-120), "20") }, "setpoint changed recently"},
		{"shift", func(h history) { h.get(entShift).add(at(-170), "-5") }, "curve changed recently"},
		{"no demand", func(h history) { h.get(entShift).add(at(-24*60+1), "-30"); h.get(entSupply).add(at(-24*60+1), "25") }, "no heat demand"},
	}
	for _, c := range cases {
		h := steadyHouse(19, 0)
		c.mutate(h)
		if s := h.evaluate(t0); s.Reason != c.want {
			t.Errorf("%s: got %q, want %q", c.name, s.Reason, c.want)
		}
	}
}

func TestEntityOf(t *testing.T) {
	for subj, want := range map[string]string{
		"homeassistant.statestream.sensor.house_temperature.state": "sensor.house_temperature",
		"homeassistant.statestream.sensor.house_temperature.unit":  "",
		"homeassistant.statestream.sensor.a.b.state":               "",
		"glow.x.SENSOR.gasmeter":                                   "",
	} {
		got, ok := entityOf(subj)
		if got != want || ok != (want != "") {
			t.Errorf("%s: got %q %v", subj, got, ok)
		}
	}
}

func TestSubject(t *testing.T) {
	if got := subject("homeassistant/sensor/heating_curve_advisor/samples/config"); got != "homeassistant.sensor.heating_curve_advisor.samples.config" {
		t.Fatal(got)
	}
}

func TestEvaluateKeepsHotWaterHours(t *testing.T) {
	h := steadyHouse(19, 0)
	h.get(entDHWGas).add(at(-20), "1.3")
	h.get(entSupply).add(at(-20), "65")
	h.get(entSupply).add(at(-10), "45")
	s := h.evaluate(t0)
	if !s.Valid || math.Abs(s.DHWGas-0.3) > 1e-9 {
		t.Fatalf("got %+v", s)
	}
}

func TestEvaluateDoesNotNeedSupply(t *testing.T) {
	h := steadyHouse(19, 0)
	h.get(entSupply).add(at(-59), "unavailable")
	if s := h.evaluate(t0); !s.Valid {
		t.Fatalf("got %+v", s)
	}
}
