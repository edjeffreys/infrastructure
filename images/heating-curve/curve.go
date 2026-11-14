package main

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// curveX is the Viessmann heating curve's outdoor term: flow = setpoint +
// shift + slope*curveX(outdoor, setpoint).
func curveX(outdoor, setpoint float64) float64 {
	d := outdoor - setpoint
	return -d * (1.4347 + 0.021*d + 0.0002479*d*d)
}

func curveFlow(slope, shift, outdoor, setpoint float64) float64 {
	return setpoint + shift + slope*curveX(outdoor, setpoint)
}

type fitConfig struct {
	HalfLife time.Duration
	BinWidth float64
	MinN     float64
	// ExcludeDHW drops hours with any hot-water gas, to compare against the HA advisor.
	ExcludeDHW bool
}

type bin struct {
	OutdoorFrom float64 `json:"outdoor_from"`
	OutdoorTo   float64 `json:"outdoor_to"`
	N           float64 `json:"n"`
	// Mean of required minus commanded flow: positive means the curve ran cold.
	Shortfall float64 `json:"shortfall"`
}

type recommendation struct {
	Status     string   `json:"status"`
	Slope      *float64 `json:"slope"`
	Shift      *int     `json:"shift"`
	N          float64  `json:"n"`
	OutdoorMin *float64 `json:"outdoor_min"`
	OutdoorMax *float64 `json:"outdoor_max"`
	ResidualSD *float64 `json:"residual_sd"`
	Bins       []bin    `json:"bins"`
}

// fit weights each sample by age, then divides by the square root of its
// outdoor bin's total weight. Mild hours vastly outnumber cold ones; without
// that the cold end of the curve is extrapolated from mild weather.
func fit(samples []sample, now time.Time, currentSlope float64, cfg fitConfig) recommendation {
	type pt struct{ x, y, w, tout, short float64 }
	binOf := func(tout float64) float64 { return math.Floor(tout/cfg.BinWidth) * cfg.BinWidth }

	var pts []pt
	binW := map[float64]float64{}
	for _, s := range samples {
		if !s.Valid || (cfg.ExcludeDHW && s.DHWGas > 0) {
			continue
		}
		age := now.Sub(s.Hour)
		if age < 0 {
			age = 0
		}
		w := math.Exp2(-age.Hours() / cfg.HalfLife.Hours())
		pts = append(pts, pt{s.X, s.Y, w, s.Outdoor, s.RequiredFlow - s.CurveFlow})
		binW[binOf(s.Outdoor)] += w
	}

	r := recommendation{Bins: []bin{}}
	var n float64
	for _, w := range binW {
		n += w
	}
	r.N = round(n, 1)
	if len(pts) == 0 {
		r.Status = "collecting (0/" + itoa(cfg.MinN) + " hours)"
		return r
	}

	lo, hi := math.Inf(1), math.Inf(-1)
	bins := map[float64]*bin{}
	for _, p := range pts {
		if p.w >= 0.05 {
			lo, hi = math.Min(lo, p.tout), math.Max(hi, p.tout)
		}
		k := binOf(p.tout)
		b := bins[k]
		if b == nil {
			b = &bin{OutdoorFrom: k, OutdoorTo: k + cfg.BinWidth}
			bins[k] = b
		}
		b.N += p.w
		b.Shortfall += p.w * p.short
	}
	keys := make([]float64, 0, len(bins))
	for k := range bins {
		keys = append(keys, k)
	}
	sort.Float64s(keys)
	for _, k := range keys {
		if b := bins[k]; b.N >= 0.05 {
			r.Bins = append(r.Bins, bin{b.OutdoorFrom, b.OutdoorTo, round(b.N, 1), round(b.Shortfall/b.N, 1)})
		}
	}
	if !math.IsInf(lo, 0) {
		r.OutdoorMin, r.OutdoorMax = ptr(round(lo, 1)), ptr(round(hi, 1))
	}

	if n < cfg.MinN {
		r.Status = "collecting (" + itoa(n) + "/" + itoa(cfg.MinN) + " hours)"
		return r
	}

	var sw, sx, sy, sxx, sxy float64
	for i := range pts {
		pts[i].w /= math.Sqrt(binW[binOf(pts[i].tout)])
		p := pts[i]
		sw += p.w
		sx += p.w * p.x
		sy += p.w * p.y
		sxx += p.w * p.x * p.x
		sxy += p.w * p.x * p.y
	}
	mx := sx / sw
	varX := sxx/sw - mx*mx

	slope := currentSlope
	// Same spread threshold as the HA advisor: below it the slope is noise.
	if varX < 4 {
		r.Status = "shift only (needs a wider outdoor range for slope)"
	} else {
		r.Status = "slope and shift"
		slope = round(clamp((sxy/sw-mx*sy/sw)/varX, 0.2, 3.5), 1)
	}
	shift := int(math.Round(clamp(sy/sw-slope*mx, -13, 40)))
	r.Slope, r.Shift = &slope, &shift

	var ss float64
	for _, p := range pts {
		e := p.y - (slope*p.x + float64(shift))
		ss += p.w * e * e
	}
	r.ResidualSD = ptr(round(math.Sqrt(ss/sw), 2))
	return r
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}

func ptr[T any](v T) *T { return &v }

func itoa(v float64) string { return strconv.Itoa(int(math.Round(v))) }
