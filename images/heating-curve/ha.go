package main

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	haDiscoveryPrefix = "homeassistant/sensor/heating_curve_advisor"
	haStateTopic      = "homeassistant/heating_curve/state"
	haAttrsTopic      = "homeassistant/heating_curve/attributes"
	haStatusSubject   = "homeassistant.status"
)

// subject maps an MQTT topic to the NATS subject nats-server's MQTT bridge uses.
func subject(topic string) string {
	return strings.ReplaceAll(strings.ReplaceAll(topic, ".", "//"), "/", ".")
}

type haSensor struct {
	key, name, template, unit, stateClass, icon string
	attributes                                  bool
}

var haSensors = []haSensor{
	{key: "status", name: "Status", template: "{{ value_json.status }}", icon: "mdi:lightbulb-on-outline", attributes: true},
	{key: "recommended_slope", name: "Recommended slope", template: "{{ value_json.slope }}", icon: "mdi:chart-bell-curve-cumulative"},
	{key: "recommended_shift", name: "Recommended shift", template: "{{ value_json.shift }}", unit: "°C", icon: "mdi:arrow-up-down"},
	{key: "samples", name: "Samples", template: "{{ value_json.n }}", stateClass: "measurement", icon: "mdi:sigma"},
	{key: "last_sample", name: "Last sample", template: "{{ value_json.sample_reason }}", icon: "mdi:test-tube"},
	{key: "required_flow", name: "Required flow", template: "{{ value_json.required_flow }}", unit: "°C", icon: "mdi:thermometer-water"},
}

// The bridge cannot retain a message published from core NATS, so configs are
// re-sent whenever HA announces itself, and states expire if the service dies.
func haDiscovery() map[string][]byte {
	device := map[string]any{
		"identifiers":  []string{"heating_curve_advisor"},
		"name":         "Heating curve advisor",
		"model":        "heating-curve",
		"manufacturer": "homelab",
	}
	out := map[string][]byte{}
	for _, s := range haSensors {
		cfg := map[string]any{
			"name":           s.name,
			"unique_id":      "heating_curve_advisor_" + s.key,
			"state_topic":    haStateTopic,
			"value_template": s.template,
			"icon":           s.icon,
			"expire_after":   int((2*time.Hour + 10*time.Minute).Seconds()),
			"device":         device,
		}
		if s.unit != "" {
			cfg["unit_of_measurement"] = s.unit
		}
		if s.stateClass != "" {
			cfg["state_class"] = s.stateClass
		}
		if s.attributes {
			cfg["json_attributes_topic"] = haAttrsTopic
		}
		b, _ := json.Marshal(cfg)
		out[subject(haDiscoveryPrefix+"/"+s.key+"/config")] = b
	}
	return out
}

type flowRow struct {
	Outdoor     float64  `json:"outdoor"`
	Current     float64  `json:"current"`
	Recommended *float64 `json:"recommended"`
}

func haPayloads(r recommendation, last *sample, slope, shift, setpoint float64, now time.Time) (state, attrs []byte) {
	st := map[string]any{
		"status":        r.Status,
		"slope":         r.Slope,
		"shift":         r.Shift,
		"n":             r.N,
		"sample_reason": nil,
		"required_flow": nil,
		"updated":       now.UTC().Format(time.RFC3339),
	}
	if last != nil {
		st["sample_reason"] = last.Reason
		if last.Valid {
			st["required_flow"] = last.RequiredFlow
		}
	}
	state, _ = json.Marshal(st)

	rows := []flowRow{}
	for _, t := range []float64{-5, 0, 5, 10, 15} {
		row := flowRow{Outdoor: t, Current: round(curveFlow(slope, shift, t, setpoint), 1)}
		if r.Slope != nil {
			row.Recommended = ptr(round(curveFlow(*r.Slope, float64(*r.Shift), t, setpoint), 1))
		}
		rows = append(rows, row)
	}
	a := map[string]any{
		"current_slope": slope,
		"current_shift": shift,
		"setpoint":      setpoint,
		"outdoor_min":   r.OutdoorMin,
		"outdoor_max":   r.OutdoorMax,
		"residual_sd":   r.ResidualSD,
		"flow_table":    rows,
		"bins":          r.Bins,
	}
	if last != nil {
		a["last_sample"] = last
	}
	attrs, _ = json.Marshal(a)
	return state, attrs
}
