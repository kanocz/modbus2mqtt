package main

import (
	"encoding/json"
	"testing"
)

func TestDiscoveryUnitsAndAvailability(t *testing.T) {
	for _, tc := range []struct{ class, unit, want string }{{"temperature", "", "°C"}, {"pressure", "kPa", "kPa"}, {"energy", "kWh", "kWh"}} {
		ha := newAutoDiscovery("homeassistant", "", "state/", "set/", "device", "Ventilation", "", "")
		ha.availabilityTopic = "availability"
		_, payload := ha.Get("sensor", "", "sensor", false, tc.class, tc.unit, nil, false)
		var entry haAutoDiscoveryEntry
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.UnitOfMeasuremet != tc.want || entry.AvailabilityTopic != "availability" {
			t.Fatalf("incorrect discovery: %s", payload)
		}
	}
}
