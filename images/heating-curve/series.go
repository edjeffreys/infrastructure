package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

type point struct {
	t   time.Time
	raw string
}

// series holds one entity's state history. Statestream publishes on change
// only, so a value holds until the next point.
type series struct {
	points []point
}

func (s *series) add(t time.Time, raw string) {
	raw = strings.TrimSpace(raw)
	i := sort.Search(len(s.points), func(i int) bool { return s.points[i].t.After(t) })
	s.points = append(s.points, point{})
	copy(s.points[i+1:], s.points[i:])
	s.points[i] = point{t, raw}
}

// prune drops points before cutoff but keeps the one in force at cutoff.
func (s *series) prune(cutoff time.Time) {
	i := sort.Search(len(s.points), func(i int) bool { return !s.points[i].t.Before(cutoff) })
	if i > 1 {
		s.points = append(s.points[:0], s.points[i-1:]...)
	}
}

func missing(raw string) bool {
	return raw == "" || raw == "unavailable" || raw == "unknown" || raw == "None"
}

func number(raw string) (float64, bool) {
	if missing(raw) {
		return 0, false
	}
	switch raw {
	case "on":
		return 1, true
	case "off":
		return 0, true
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// at returns the raw state in force at t.
func (s *series) at(t time.Time) (string, bool) {
	i := sort.Search(len(s.points), func(i int) bool { return s.points[i].t.After(t) })
	if i == 0 {
		return "", false
	}
	return s.points[i-1].raw, true
}

func (s *series) numberAt(t time.Time) (float64, bool) {
	raw, ok := s.at(t)
	if !ok {
		return 0, false
	}
	return number(raw)
}

// segments calls fn for each stretch of [from, to) with a numeric value.
func (s *series) segments(from, to time.Time, fn func(start, end time.Time, v float64)) {
	for i := range s.points {
		start := s.points[i].t
		end := to
		if i+1 < len(s.points) {
			end = s.points[i+1].t
		}
		if !end.After(from) || !start.Before(to) {
			continue
		}
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if v, ok := number(s.points[i].raw); ok && end.After(start) {
			fn(start, end, v)
		}
	}
}

// mean is time-weighted: sensors report unevenly, and a plain mean of the
// points would weight bursts of reports. It needs data for half the window.
func (s *series) mean(from, to time.Time) (float64, bool) {
	var sum, covered float64
	s.segments(from, to, func(start, end time.Time, v float64) {
		d := end.Sub(start).Seconds()
		sum += v * d
		covered += d
	})
	if covered < to.Sub(from).Seconds()/2 {
		return 0, false
	}
	return sum / covered, true
}

func (s *series) max(from, to time.Time) (float64, bool) {
	m, ok := math.Inf(-1), false
	s.segments(from, to, func(_, _ time.Time, v float64) {
		m, ok = math.Max(m, v), true
	})
	return m, ok
}

// rise sums the increases over [from, to), so a daily counter's midnight
// reset to zero does not read as use.
func (s *series) rise(from, to time.Time) float64 {
	prev, have := s.numberAt(from)
	var total float64
	for _, p := range s.points {
		if !p.t.After(from) || !p.t.Before(to) {
			continue
		}
		v, ok := number(p.raw)
		if !ok {
			continue
		}
		if have && v > prev {
			total += v - prev
		}
		prev, have = v, true
	}
	return total
}

// lastChange is when the value last became different from the one before
// it, at or before t. Unavailable blips and HA's restart republish of an
// unchanged value do not count.
func (s *series) lastChange(t time.Time) (time.Time, bool) {
	var last time.Time
	var prev string
	found := false
	for _, p := range s.points {
		if p.t.After(t) {
			break
		}
		if missing(p.raw) {
			continue
		}
		if !found || p.raw != prev {
			last = p.t
		}
		prev, found = p.raw, true
	}
	return last, found
}
