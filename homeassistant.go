package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

type haAutoDiscoveryEntry struct {
	Name              string   `json:"name"`
	UniqueID          string   `json:"unique_id"`
	StateTopic        string   `json:"state_topic"`
	AvailabilityTopic string   `json:"availability_topic,omitempty"`
	CommandTopic      string   `json:"command_topic,omitempty"`
	DeviceClass       string   `json:"device_class,omitempty"`
	UnitOfMeasuremet  string   `json:"unit_of_measurement,omitempty"`
	Step              float64  `json:"step,omitempty"`
	Min               float64  `json:"min,omitempty"`
	Max               float64  `json:"max,omitempty"`
	Mode              string   `json:"mode,omitempty"`
	Options           []string `json:"options,omitempty"`
	Device            struct {
		Identifiers  []string `json:"identifiers"`
		Name         string   `json:"name"`
		Model        string   `json:"model,omitempty"`
		Manufacturer string   `json:"manufacturer,omitempty"`
	} `json:"device,omitempty"`
	PayloadOn  string `json:"payload_on,omitempty"`
	PayloadOff string `json:"payload_off,omitempty"`
	Retain     bool   `json:"retain,omitempty"`
}

type haAutoDiscovery struct {
	autoDiscoveryTopic string
	availabilityTopic  string
	stateTopic         string
	setTopic           string
	namePrefix         string
	deviceID           string
	deviceName         string
	model              string
	manufacturer       string
}

func newAutoDiscovery(autoDiscoveryTopic string, namePrefix string, stateTopic string, setTopic string,
	devID string, devName string, manufacturer string, model string) haAutoDiscovery {
	if namePrefix != "" && !strings.HasSuffix(namePrefix, "_") {
		namePrefix += "_"
	}
	return haAutoDiscovery{
		autoDiscoveryTopic: autoDiscoveryTopic,
		namePrefix:         namePrefix,
		stateTopic:         stateTopic,
		setTopic:           setTopic,
		deviceID:           devID,
		deviceName:         devName,
		model:              model,
		manufacturer:       manufacturer,
	}
}

func (haad haAutoDiscovery) Get(name string, origName string, eType string, writable bool,
	haClass string, haUnit string, values map[uint16]string, x10 bool) (topic string, payload string) {

	adName := haad.namePrefix + normalizeName(name)

	if haUnit == "" {
		switch haClass {
		case "temperature":
			haUnit = "°C"
		case "humidity":
			haUnit = "%"
		case "pressure":
			haUnit = "Pa"
		case "illuminance":
			haUnit = "lx"
		case "power":
			haUnit = "W"
		case "hour", "hours":
			haUnit = "h"
		case "minute", "minutes":
			haUnit = "min"
		case "second", "seconds":
			haUnit = "s"
		}
	}

	if haClass == "" && haUnit == "" {
		if strings.HasSuffix(name, "_rpm") {
			haUnit = "rpm"
		}
		if strings.HasSuffix(name, "_pct") {
			haUnit = "%"
		}
		if strings.HasSuffix(name, "_flow_m3_h") {
			haClass = "volume_flow_rate"
			haUnit = "m³/h"
		}
		if len(values) > 0 {
			haClass = "enum"
		}

	}

	// if origName is empty, use name
	if origName == "" {
		origName = name
	}

	// special hack for IR_ generated names, remove IR_ prefix
	origName = strings.TrimPrefix(origName, "IR_")

	// if it's not spaces in origName and it's not lowercase, convert it to lowercase and replace "_" to " "
	if !strings.Contains(origName, " ") && strings.ToUpper(origName) == origName {
		origName = strings.ReplaceAll(strings.ToLower(origName), "_", " ")
	}

	haadEntry := haAutoDiscoveryEntry{
		DeviceClass:       haClass,
		UnitOfMeasuremet:  haUnit,
		StateTopic:        haad.stateTopic + normalizeName(name),
		AvailabilityTopic: haad.availabilityTopic,
		UniqueID:          adName,
		Name:              origName,
		Device: struct {
			Identifiers  []string `json:"identifiers"`
			Name         string   `json:"name"`
			Model        string   `json:"model,omitempty"`
			Manufacturer string   `json:"manufacturer,omitempty"`
		}{
			Identifiers:  []string{haad.deviceID},
			Name:         haad.deviceName,
			Model:        haad.model,
			Manufacturer: haad.manufacturer,
		},
	}

	if haClass == "hour" || haClass == "hours" {
		haadEntry.Min = 0
		haadEntry.Max = 23
		haadEntry.Step = 1
		haadEntry.Mode = "box"
		haadEntry.DeviceClass = ""
	}
	if haClass == "minute" || haClass == "minutes" || haClass == "second" || haClass == "seconds" {
		haadEntry.Min = 0
		haadEntry.Max = 59
		haadEntry.Step = 1
		haadEntry.Mode = "box"
		haadEntry.DeviceClass = ""
	}

	if eType == "switch" || eType == "binary_sensor" {
		haadEntry.PayloadOn = "on"
		haadEntry.PayloadOff = "off"
	}

	if x10 {
		haadEntry.Step = 0.1
	}

	if writable {
		haadEntry.CommandTopic = haad.setTopic + normalizeName(name)
	}

	if len(values) > 0 && eType == "select" {
		// prepare values for select
		for _, v := range values {
			haadEntry.Options = append(haadEntry.Options, v)
		}
	}

	payloadB, err := json.Marshal(haadEntry)
	if err != nil {
		log.Panic("unable to marshal haAutoDiscoveryEntry: ", err)
	}

	topic = fmt.Sprintf("%s/%s/%s/config", haad.autoDiscoveryTopic, eType, adName)
	payload = string(payloadB)

	return
}
