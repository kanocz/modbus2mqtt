package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type configStructRegistersData struct {
	Name     string            `json:"name"`
	origName string            // original name
	Writable bool              `json:"writable"`
	Type     string            `json:"type"`
	Pos      int               `json:"position"`
	maxPos   int               // max position for this register
	Width    int               `json:"width"`
	Values   map[uint16]string `json:"values"`
	ValueX10 bool              `json:"x10"`
	Signed   bool              `json:"signed"`
	Daily    bool              `json:"daily"`
	last     time.Time         // last time this register was updated
	HAType   string            `json:"homeAssistantType"`
	HAClass  string            `json:"homeAssistantClass"`
	HAUnit   string            `json:"homeAssistantUnit"`
}

type configStructRegisters struct {
	Reg  uint16                      `json:"reg"`
	Type string                      `json:"type"`
	Data []configStructRegistersData `json:"data"`
}

type configStruct struct {
	Registers []configStructRegisters `json:"registers"`
	MQTT      struct {
		Lwt_topic   string `json:"lwt_topic"`
		State_topic string `json:"state_topic"`
		Set_topic   string `json:"set_topic"`
		Broker      string `json:"broker"`
		Username    string `json:"username"`
		Password    string `json:"password"`
		HATopic     string `json:"homeAssistantTopic"`
		HAPrefix    string `json:"homeAssistantPrefix"`
		HAdevID     string `json:"homeAssistantDeviceID"`
		HAdevName   string `json:"homeAssistantDeviceName"`
		HAManu      string `json:"homeAssistantDeviceManufacturer"`
		HAModel     string `json:"homeAssistantDeviceModel"`
	} `json:"mqtt"`
}

var (
	config     configStruct
	configFile = flag.String("config", "modbus2mqtt.json", "modbus2mqtt config in json format")
)

func readConfig() error {

	file, err := os.ReadFile(*configFile)
	if nil != err {
		return err
	}

	var cfg configStruct
	err = json.Unmarshal(file, &cfg)
	if nil != err {
		return err
	}

	if cfg.MQTT.Lwt_topic == "" || cfg.MQTT.Set_topic == "" || cfg.MQTT.State_topic == "" {
		return errors.New("MQTT config incomplete")
	}
	for _, topic := range []string{cfg.MQTT.Lwt_topic, cfg.MQTT.Set_topic, cfg.MQTT.State_topic, cfg.MQTT.HATopic} {
		if strings.ContainsAny(topic, "+#\x00") {
			return fmt.Errorf("invalid MQTT topic %q", topic)
		}
	}
	if err := cfg.prepareRegisters(); err != nil {
		return err
	}
	config = cfg
	return nil
}

func (cfg *configStruct) prepareRegisters() error {
	// Each client owns its polling timestamps, even when created from the same config.
	cfg.Registers = append([]configStructRegisters(nil), cfg.Registers...)
	names := make(map[string]bool)
	addName := func(name string) error {
		name = normalizeName(name)
		if strings.Trim(name, "_") == "" || names[name] {
			return fmt.Errorf("empty or duplicate normalized name %q", name)
		}
		names[name] = true
		return nil
	}
	for i := range cfg.Registers {
		reg := &cfg.Registers[i]
		if reg.Type != "hold" && reg.Type != "input" && reg.Type != "coils" {
			return fmt.Errorf("register %d: unknown type %q", reg.Reg, reg.Type)
		}
		if len(reg.Data) == 0 {
			return fmt.Errorf("register %d: no data fields", reg.Reg)
		}
		reg.Data = append([]configStructRegistersData(nil), reg.Data...)
		for j := range reg.Data {
			data := &reg.Data[j]
			if data.origName == "" {
				data.origName = data.Name
			}
			data.Name = normalizeName(data.Name)
			data.last = time.Time{}
			if reg.Type == "input" && data.Writable {
				return fmt.Errorf("field %q: input registers cannot be writable", data.Name)
			}
			if reg.Type == "coils" {
				if data.Type != "coils" || len(data.Values) == 0 {
					return fmt.Errorf("coils group %d: expected coils type and nonempty values", j)
				}
				data.Pos, data.maxPos = 65535, 0
				for addr, name := range data.Values {
					if err := addName(name); err != nil {
						return fmt.Errorf("coil %d: %w", addr, err)
					}
					data.Pos = min(data.Pos, int(addr))
					data.maxPos = max(data.maxPos, int(addr))
				}
				if data.maxPos-data.Pos+1 > 2000 {
					return fmt.Errorf("coils group %d: range exceeds Modbus limit of 2000 coils; split the group", j)
				}
				continue
			}
			if err := addName(data.Name); err != nil {
				return err
			}
			switch data.Type {
			case "bit":
				data.Width = 1
			case "bits", "bits_const":
			default:
				return fmt.Errorf("field %q: unknown type %q", data.Name, data.Type)
			}
			if data.Pos < 0 || data.Pos > 15 || data.Width < 1 || data.Width > 16-data.Pos {
				return fmt.Errorf("field %q: invalid position %d / width %d", data.Name, data.Pos, data.Width)
			}
			if data.Type != "bits" && (data.Signed || data.ValueX10) {
				return fmt.Errorf("field %q: signed and x10 require bits type", data.Name)
			}
			if data.Type == "bits_const" {
				if len(data.Values) == 0 {
					return fmt.Errorf("field %q: missing enum values", data.Name)
				}
				labels := make(map[string]bool)
				for value, label := range data.Values {
					if uint32(value) >= uint32(1)<<data.Width || (data.Writable && labels[label]) {
						return fmt.Errorf("field %q: invalid or ambiguous enum value %d", data.Name, value)
					}
					labels[label] = true
				}
			}
		}
	}
	return nil
}
