package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

var (
	modbusEP              = flag.String("modbus.connect", "tcp://192.168.1.21:502", "tcp / rtu / rtuovertcp / udp / rtuoverudp / ...")
	modbusTimeout         = flag.Duration("modbus.timeout", time.Second*5, "modbus timeout")
	modbusScan            = flag.Duration("modbus.scan", time.Second*10, "modbus scan loop duration")
	modbusPause           = flag.Duration("modbus.pause", time.Millisecond*50, "modbus pause between reads")
	modbusStartupAttempts = flag.Int("modbus.startup-attempts", 3, "total Modbus connection attempts at startup (at least 1), separated by modbus.scan")
	mqttBroker            = flag.String("mqtt.broker", "", "[scheme://]ip[:port] of mqtt broker")
	mqttUser              = flag.String("mqtt.username", "", "username for mqtt broker")
	mqttPassword          = flag.String("mqtt.password", "", "password for mqtt broker")
	mqttTimeout           = flag.Duration("mqtt.timeout", time.Second*5, "mqtt connect timeout")
	mqttResyncInterval    = flag.Duration("mqtt.resync-interval", 5*time.Minute, "minimum interval between full MQTT discovery/state resends after reconnect")
)

func main() {

	log.Println("modbus2mqtt starting")

	flag.Parse()
	if *modbusPause < 0 {
		log.Fatal("modbus.pause must not be negative")
	}
	if *modbusStartupAttempts < 1 {
		log.Fatal("modbus.startup-attempts must be at least 1")
	}

	err := readConfig()

	if err != nil {
		log.Panic("unable to read config: ", err)
	}

	mqtt_pass := config.MQTT.Password
	if *mqttPassword != "" {
		mqtt_pass = *mqttPassword
	}
	mqtt_user := config.MQTT.Username
	if *mqttUser != "" {
		mqtt_user = *mqttUser
	}
	mqtt_broker := config.MQTT.Broker
	if *mqttBroker != "" {
		mqtt_broker = *mqttBroker
	}

	mqttapi, err := NewMqttAPI(mqtt_broker, mqtt_user, mqtt_pass, *mqttTimeout, *mqttResyncInterval,
		config.MQTT.Lwt_topic, config.MQTT.State_topic, config.MQTT.Set_topic)
	if err != nil {
		log.Panic("unable to create mqtt client: ", err)
	}
	defer mqttapi.Close()

	mapi, err := NewModbusAPI(config, *modbusEP, *modbusTimeout, *modbusScan, *modbusStartupAttempts, func(s string, u uint16) {
		log.Printf("%s: %v", s, u)
		mqttapi.Publish(s, strconv.FormatUint(uint64(u), 10))
	}, func(s1, s2 string) {
		log.Printf("%s: %v", s1, s2)
		mqttapi.Publish(s1, s2)
	}, func(s string, b bool) {
		log.Printf("%s: %v", s, b)
		v := "off"
		if b {
			v = "on"
		}
		mqttapi.Publish(s, v)
	}, func(s string, v string) {
		log.Printf("raw %s: %v", s, v)
		mqttapi.RawPublish(s, v)
	})
	if err != nil {
		log.Panic("unable to create modbus client: ", err)
	}
	defer func() {
		mqttapi.Close()
		mapi.Close()
	}()

	mqttapi.SetCB(func(name, value string) {
		err := mapi.Set(name, value)
		if err == nil {
			log.Printf(" set '%s'='%s' OK\n", name, value)
		} else {
			log.Printf(" set '%s'='%s' error: %+v\n", name, value, err)
		}
	})
	mqttapi.Online()

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGTERM)
	signal.Notify(exitChan, syscall.SIGINT)
	<-exitChan
	log.Println("Exiting...")
}
