package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestCurveFlowMatchesDashboard(t *testing.T) {
	cases := []struct {
		slope, shift float64
		want         []float64
	}{
		{1.6, -6, []float64{54.2, 47.2, 39.6, 31.2, 21.7}},
		{1.4, -4, []float64{51.1, 44.9, 38.3, 30.9, 22.6}},
	}
	for _, c := range cases {
		for i, tout := range []float64{-5, 0, 5, 10, 15} {
			if got := round(curveFlow(c.slope, c.shift, tout, 19), 1); got != c.want[i] {
				t.Errorf("%v/%v at %v °C: got %v, want %v", c.slope, c.shift, tout, got, c.want[i])
			}
		}
	}
}

func synthetic(slope, shift float64, outdoors []float64, end time.Time) []sample {
	var out []sample
	for i, tout := range outdoors {
		x := curveX(tout, 19)
		y := slope*x + shift
		out = append(out, sample{
			Hour: end.Add(-time.Duration(len(outdoors)-i) * time.Hour), Valid: true, Reason: "ok",
			Outdoor: tout, Setpoint: 19, X: x, Y: y, RequiredFlow: y + 19, CurveFlow: curveFlow(1.6, -6, tout, 19),
		})
	}
	return out
}

var testFit = fitConfig{HalfLife: 30 * 24 * time.Hour, BinWidth: 2, MinN: 24}

func TestFitRecoversCurve(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var outdoors []float64
	for i := 0; i < 200; i++ {
		outdoors = append(outdoors, -3+float64(i%15))
	}
	r := fit(synthetic(1.4, -4, outdoors, now), now, 1.6, testFit)
	if r.Status != "slope and shift" || r.Slope == nil || *r.Slope != 1.4 || *r.Shift != -4 {
		t.Fatalf("got %+v", r)
	}
	if *r.ResidualSD > 0.5 {
		t.Errorf("residual sd %v", *r.ResidualSD)
	}
	for _, b := range r.Bins {
		// Current curve 1.6/-6 runs hot of 1.4/-4 below about 10 °C.
		if b.OutdoorTo <= 8 && b.Shortfall >= 0 {
			t.Errorf("bin %+v: want a negative shortfall", b)
		}
	}
}

func TestFitNarrowRangeIsShiftOnly(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var outdoors []float64
	for i := 0; i < 48; i++ {
		outdoors = append(outdoors, 8+float64(i%2))
	}
	r := fit(synthetic(1.4, -4, outdoors, now), now, 1.6, testFit)
	if !strings.HasPrefix(r.Status, "shift only") || *r.Slope != 1.6 {
		t.Fatalf("got %+v", r)
	}
}

func TestFitCollecting(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	r := fit(synthetic(1.4, -4, []float64{1, 2, 3}, now), now, 1.6, testFit)
	if r.Status != "collecting (3/24 hours)" || r.Slope != nil {
		t.Fatalf("got %+v", r)
	}
	if r := fit(nil, now, 1.6, testFit); r.Status != "collecting (0/24 hours)" {
		t.Fatalf("got %+v", r)
	}
}

// Many mild hours from one curve and a few cold ones from a steeper one: the
// bin weighting should pull the fit toward the cold evidence more than a
// plain least-squares fit would.
func TestFitBinWeightingFavoursSparseColdEnd(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var mild []float64
	for i := 0; i < 300; i++ {
		mild = append(mild, 6+float64(i%7))
	}
	samples := synthetic(1.3, -3, mild, now.Add(-10*time.Hour))
	samples = append(samples, synthetic(1.3, 1, []float64{-1, 0, 0, 1, 1, 0, -1, 0, -1, 0, 0, 1, 1, 0, -1, 0}, now)...)
	r := fit(samples, now, 1.6, testFit)
	cold := curveFlow(*r.Slope, float64(*r.Shift), 0, 19)
	plain := fit(samples, now, 1.6, fitConfig{HalfLife: testFit.HalfLife, BinWidth: 1000, MinN: 24})
	coldPlain := curveFlow(*plain.Slope, float64(*plain.Shift), 0, 19)
	if !(cold > coldPlain) {
		t.Fatalf("binned fit at 0 °C %.1f, plain %.1f: want binned higher", cold, coldPlain)
	}
	if math.IsNaN(cold) {
		t.Fatal("nan")
	}
}

func TestOldSamplesDecay(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var outdoors []float64
	for i := 0; i < 48; i++ {
		outdoors = append(outdoors, float64(i%12))
	}
	old := synthetic(1.4, -4, outdoors, now.Add(-90*24*time.Hour))
	r := fit(old, now, 1.6, testFit)
	if r.N > 7 || !strings.HasPrefix(r.Status, "collecting") {
		t.Fatalf("90-day-old samples should weigh about 1/8: got %+v", r)
	}
}

func TestFitExcludeDHW(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var outdoors []float64
	for i := 0; i < 48; i++ {
		outdoors = append(outdoors, float64(i%12))
	}
	samples := synthetic(1.4, -4, outdoors, now)
	for i := range samples[:30] {
		samples[i].DHWGas = 0.2
	}
	cfg := testFit
	cfg.ExcludeDHW = true
	if r := fit(samples, now, 1.6, cfg); !strings.HasPrefix(r.Status, "collecting (18/") {
		t.Fatalf("got %+v", r)
	}
	if r := fit(samples, now, 1.6, testFit); r.Status != "slope and shift" {
		t.Fatalf("got %+v", r)
	}
}
