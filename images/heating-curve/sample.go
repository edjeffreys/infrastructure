package main

import (
	"math"
	"time"
)

const (
	entHouse     = "sensor.house_temperature"
	entOutdoor   = "sensor.boiler_outside_temperature"
	entSupply    = "sensor.boiler_supply_temperature"
	entDHWGas    = "sensor.boiler_dhw_gas_consumption_today"
	entSetpoint  = "sensor.heating_active_setpoint"
	entSlope     = "number.boiler_heating_curve_slope"
	entShift     = "number.boiler_heating_curve_shift"
	entClimate   = "climate.boiler_heating"
	entSolarGain = "binary_sensor.recent_solar_gain"
	entTarget    = "input_number.heating_target_normal"
)

var entities = []string{entHouse, entOutdoor, entSupply, entDHWGas, entSetpoint, entSlope, entShift, entClimate, entSolarGain, entTarget}

var tracked = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range entities {
		m[e] = true
	}
	return m
}()

// halfDT is the assumed flow-return split, so mean water temperature is flow - halfDT.
const halfDT = 5.0

// radiatorExponent is the n in radiator output ∝ (MWT - room)^n.
const radiatorExponent = 1.3

// trendWindow spans the first and last hour means the indoor trend is taken from.
const trendWindow = 3 * time.Hour

// maxTrend rejects transients (doors, sun, reheats) that a single time constant
// extrapolates badly; at 60 h it already means heading 6 °C away.
const maxTrend = 0.1

// sample is one hour's evidence: the flow temperature that would have held
// the setpoint, given where the house was heading on the flow it actually got.
// Invalid hours are kept, with the reason, so gaps can be explained.
type sample struct {
	Hour    time.Time `json:"hour"`
	Valid   bool      `json:"valid"`
	Reason  string    `json:"reason"`
	Indoor  float64   `json:"indoor"`
	Outdoor float64   `json:"outdoor"`
	Supply  float64   `json:"supply"`
	Trend   float64   `json:"trend"`
	// Heading is where the house would settle on this hour's flow.
	Heading      float64 `json:"heading"`
	DHWGas       float64 `json:"dhw_gas"`
	Setpoint     float64 `json:"setpoint"`
	Slope        float64 `json:"slope"`
	Shift        float64 `json:"shift"`
	CurveFlow    float64 `json:"curve_flow"`
	RequiredFlow float64 `json:"required_flow"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
}

type history map[string]*series

func (h history) get(entity string) *series {
	s := h[entity]
	if s == nil {
		s = &series{}
		h[entity] = s
	}
	return s
}

// evaluate computes the sample for the hour ending at end, from the measured
// supply and where the house was heading rather than where it was: a house
// coasting down from a sunny afternoon is not evidence the curve runs hot.
func (h history) evaluate(end time.Time, tau time.Duration) sample {
	from := end.Add(-time.Hour)
	s := sample{Hour: end}

	tin, ok1 := h.get(entHouse).mean(from, end)
	tout, ok2 := h.get(entOutdoor).mean(from, end)
	flow, ok3 := h.get(entSupply).mean(from, end)
	tset, ok4 := h.get(entSetpoint).numberAt(end)
	slope, ok5 := h.get(entSlope).numberAt(end)
	shift, ok6 := h.get(entShift).numberAt(end)
	tinBefore, ok7 := h.get(entHouse).mean(end.Add(-trendWindow), end.Add(-trendWindow+time.Hour))
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7) {
		s.Reason = "missing data"
		return s
	}
	trend := (tin - tinBefore) / (trendWindow - time.Hour).Hours()
	heading := tin + tau.Hours()*trend
	s.Indoor, s.Outdoor, s.Supply = round(tin, 2), round(tout, 2), round(flow, 2)
	s.Trend, s.Heading = round(trend, 3), round(heading, 2)
	s.Setpoint, s.Slope, s.Shift = tset, slope, shift
	s.DHWGas = round(h.get(entDHWGas).rise(from, end), 3)

	curve := curveFlow(slope, shift, tout, tset)
	mwt := flow - halfDT
	s.CurveFlow = round(curve, 1)

	climate, _ := h.get(entClimate).at(end)
	solar, _ := h.get(entSolarGain).at(end)
	setpointChanged, _ := h.get(entSetpoint).lastChange(end)
	curveChanged, _ := h.get(entSlope).lastChange(end)
	if t, ok := h.get(entShift).lastChange(end); ok && t.After(curveChanged) {
		curveChanged = t
	}

	switch {
	case climate != "auto":
		s.Reason = "heating off"
	case solar == "on":
		s.Reason = "solar gain"
	case tout > 12:
		s.Reason = "too mild: boiler at minimum output"
	case end.Sub(setpointChanged) < 3*time.Hour:
		s.Reason = "setpoint changed recently"
	case end.Sub(curveChanged) < 3*time.Hour:
		s.Reason = "curve changed recently"
	// The supply sensor is shared with the cylinder, and a reheat starves the
	// radiators, so the hour and the trend window behind it are both unusable.
	case h.get(entDHWGas).rise(end.Add(-trendWindow), end) > 0:
		s.Reason = "hot water"
	case math.Abs(trend) > maxTrend:
		s.Reason = "indoor changing fast"
	case tin-tout < 3 || heading-tout < 3 || mwt-tin < 3:
		s.Reason = "no heat demand"
	default:
		// Radiator output is K(mwt-tin)^n and equals UA(heading-tout), so
		// holding tset needs K(mwtReq-tset)^n = UA(tset-tout).
		mwtReq := tset + (mwt-tin)*math.Pow((tset-tout)/(heading-tout), 1/radiatorExponent)
		s.Valid, s.Reason = true, "ok"
		s.X = round(curveX(tout, tset), 3)
		s.Y = round(mwtReq+halfDT-tset, 3)
		s.RequiredFlow = round(mwtReq+halfDT, 1)
	}
	return s
}

func (h history) prune(cutoff time.Time) {
	for _, s := range h {
		s.prune(cutoff)
	}
}
