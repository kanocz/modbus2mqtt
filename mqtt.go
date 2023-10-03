package main

import (
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

var (
	errMqttTimeout = errors.New("mqtt connect timeout")
)

type mqttAPI struct {
	client mqtt.Client
	cb     func(string, string)
	mu     sync.RWMutex

	lwt_topic   string
	state_topic string
	set_topic   string
}

func (api *mqttAPI) msgHandler(c mqtt.Client, m mqtt.Message) {

	topic := m.Topic()
	payload := string(m.Payload())

	api.mu.RLock()
	defer api.mu.RUnlock()

	if api.cb == nil {
		return
	}

	if !strings.HasPrefix(topic, api.set_topic) {
		return
	}

	name := topic[len(api.set_topic):]
	if len(name) < 1 {
		return
	}

	go api.cb(name, payload)
}

// set callback for incoming messages
func (api *mqttAPI) SetCB(cb func(string, string)) {
	api.mu.Lock()
	defer api.mu.Unlock()

	api.cb = cb
}

// publish "online" message
func (api *mqttAPI) Online() {
	api.client.Publish(api.lwt_topic, 1, false, "online")
}

func (api *mqttAPI) Publish(name string, value interface{}) {
	api.client.Publish(api.state_topic+name, 1, true, value)
}

func NewMqttAPI(mqttBroker, mqttUser, mqttPassword string, mqttTimeout time.Duration, lwt_topic string, state_topic string, set_topic string) (*mqttAPI, error) {

	options := mqtt.NewClientOptions()
	options.Username = mqttUser
	options.Password = mqttPassword
	options.Order = false
	options.AddBroker(mqttBroker)
	options.ClientID = "modbus2mqtt_" + strconv.FormatInt(int64(os.Getpid()), 10)
	options.AutoReconnect = true
	options.ResumeSubs = false
	options.DefaultPublishHandler = func(c mqtt.Client, m mqtt.Message) {}

	client := mqtt.NewClient(options)
	t := client.Connect()

	if !t.WaitTimeout(mqttTimeout) {
		return nil, errMqttTimeout
	}
	if t.Error() != nil {
		return nil, t.Error()
	}

	api := &mqttAPI{
		client:      client,
		lwt_topic:   lwt_topic,
		state_topic: state_topic,
		set_topic:   set_topic,
	}

	log.Printf("Subscribe to mqtt topic '%s'", set_topic+"#")
	t = api.client.Subscribe(set_topic+"#", 1, api.msgHandler)
	if !t.WaitTimeout(mqttTimeout) {
		client.Disconnect(0)
		return nil, errMqttTimeout
	}

	if t.Error() != nil {
		return nil, t.Error()
	}

	api.client.Publish(lwt_topic, 0, false, "offline")

	return api, nil

}
