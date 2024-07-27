package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
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
	rValues  map[string]uint16 // reverse values for faster lookup
	ValueX10 bool              `json:"x10"`
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

	err = json.Unmarshal(file, &config)
	if nil != err {
		return err
	}

	if config.MQTT.Lwt_topic == "" || config.MQTT.Set_topic == "" || config.MQTT.State_topic == "" {
		return errors.New("MQTT config incomplete")
	}

	// use normalizeName for all names and count max pos + create reverse values map
	for i, reg := range config.Registers {
		for j, data := range reg.Data {
			config.Registers[i].Data[j].origName = config.Registers[i].Data[j].Name
			config.Registers[i].Data[j].Name = normalizeName(data.Name)
			config.Registers[i].Data[j].rValues = make(map[string]uint16)
			if data.Type == "coils" {
				for l := range data.Values {
					config.Registers[i].Data[j].maxPos = max(config.Registers[i].Data[j].maxPos, int(l))
					if config.Registers[i].Data[j].Pos == 0 {
						config.Registers[i].Data[j].Pos = int(l)
					} else {
						config.Registers[i].Data[j].Pos = min(config.Registers[i].Data[j].Pos, int(l))
					}
				}
			}
			if data.Type == "bits_const" {
				for l, v := range data.Values {
					config.Registers[i].Data[j].rValues[v] = l
				}
			}

		}
	}

	return nil
}
