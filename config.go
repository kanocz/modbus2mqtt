package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
)

type configStructRegistersData struct {
	Name     string            `json:"name"`
	Writable bool              `json:"writable"`
	Type     string            `json:"type"`
	Pos      int               `json:"position"`
	Width    int               `json:"width"`
	Values   map[uint16]string `json:"values"`
	ValueX10 bool              `json:"x10"`
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

	return nil
}
