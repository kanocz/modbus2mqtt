package main

import (
	"errors"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errMqttTimeout = errors.New("mqtt operation timed out")

type mqttCommand struct{ name, value string }

type mqttAPI struct {
	client                            mqtt.Client
	mu                                sync.Mutex
	cb                                func(string, string)
	lwt_topic, state_topic, set_topic string
	timeout, resyncInterval           time.Duration
	retained                          map[string]string
	pending                           map[string]bool
	connected, needsSubscribe         bool
	generation                        uint64
	resyncRequested                   bool
	lastResync                        time.Time
	wake                              chan struct{}
	done                              chan struct{}
	commands                          chan mqttCommand
	cbReady                           chan struct{}
	initial                           chan error
	cbOnce, initOnce, closeOnce       sync.Once
	wg                                sync.WaitGroup
}

func (api *mqttAPI) signal() {
	select {
	case api.wake <- struct{}{}:
	default:
	}
}

func (api *mqttAPI) msgHandler(_ mqtt.Client, m mqtt.Message) {
	if !strings.HasPrefix(m.Topic(), api.set_topic) {
		return
	}
	name := strings.TrimPrefix(m.Topic(), api.set_topic)
	if name == "" {
		return
	}
	// Keep the MQTT reader free to process acknowledgements and keepalives.
	select {
	case <-api.done:
	case api.commands <- mqttCommand{name, string(m.Payload())}:
	default:
		log.Printf("MQTT command queue full; rejected command for %q", name)
	}
}

func (api *mqttAPI) SetCB(cb func(string, string)) {
	api.mu.Lock()
	api.cb = cb
	api.mu.Unlock()
	if cb != nil {
		api.cbOnce.Do(func() { close(api.cbReady) })
	}
}

func (api *mqttAPI) commandLoop() {
	defer api.wg.Done()
	select {
	case <-api.done:
		return
	case <-api.cbReady:
	}
	for {
		select {
		case <-api.done:
			return
		case command := <-api.commands:
			select {
			case <-api.done:
				return
			default:
			}
			api.mu.Lock()
			cb := api.cb
			api.mu.Unlock()
			if cb != nil {
				cb(command.name, command.value)
			}
		}
	}
}

func (api *mqttAPI) Online()                    { api.RawPublish(api.lwt_topic, "online") }
func (api *mqttAPI) Publish(name, value string) { api.RawPublish(api.state_topic+name, value) }

func (api *mqttAPI) RawPublish(topic, payload string) {
	api.mu.Lock()
	api.retained[topic] = payload
	api.pending[topic] = true
	api.mu.Unlock()
	api.signal()
}

func (api *mqttAPI) onConnect(_ mqtt.Client) {
	api.mu.Lock()
	api.connected = api.client.IsConnectionOpen()
	api.needsSubscribe = true
	api.generation++
	if api.lastResync.IsZero() {
		api.lastResync = time.Now()
	} else {
		api.resyncRequested = true
	}
	api.pending[api.lwt_topic] = true
	api.mu.Unlock()
	api.signal()
}

func waitMQTT(token mqtt.Token, timeout time.Duration) error {
	if !token.WaitTimeout(timeout) {
		return errMqttTimeout
	}
	return token.Error()
}

// A single publisher keeps only the latest value per topic and retries failed sends.
func (api *mqttAPI) publishLoop() {
	defer api.wg.Done()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-api.done:
			return
		case <-api.wake:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if delay := api.flush(); delay > 0 {
			timer.Reset(delay)
		}
	}
}

func (api *mqttAPI) flush() time.Duration {
	select {
	case <-api.done:
		return 0
	default:
	}
	api.mu.Lock()
	connected, subscribe, generation := api.connected, api.needsSubscribe, api.generation
	api.mu.Unlock()
	if !connected {
		return 0
	}
	if subscribe {
		err := waitMQTT(api.client.Subscribe(api.set_topic+"#", 1, api.msgHandler), api.timeout)
		api.initOnce.Do(func() { api.initial <- err })
		if err != nil {
			log.Printf("MQTT subscribe: %v; retrying in %s", err, api.timeout)
			return api.timeout
		}
		api.mu.Lock()
		if api.generation == generation {
			api.needsSubscribe = false
		}
		api.mu.Unlock()
	}
	api.mu.Lock()
	if !api.connected || api.generation != generation {
		api.mu.Unlock()
		return 0
	}
	var delay time.Duration
	if api.resyncRequested {
		delay = time.Until(api.lastResync.Add(api.resyncInterval))
		if delay <= 0 {
			for topic := range api.retained {
				api.pending[topic] = true
			}
			api.lastResync = time.Now()
			api.resyncRequested = false
			log.Println("MQTT resending discovery and cached states")
		}
	}
	values := make(map[string]string, len(api.pending))
	for topic := range api.pending {
		values[topic] = api.retained[topic]
	}
	api.mu.Unlock()
	topics := make([]string, 0, len(values))
	for topic := range values {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	for _, topic := range topics {
		select {
		case <-api.done:
			return 0
		default:
		}
		if err := waitMQTT(api.client.Publish(topic, 1, true, values[topic]), api.timeout); err != nil {
			log.Printf("MQTT publish %q: %v; retrying in %s", topic, err, api.timeout)
			return api.timeout
		}
		api.mu.Lock()
		if api.connected && api.generation == generation && api.retained[topic] == values[topic] {
			delete(api.pending, topic)
		}
		api.mu.Unlock()
	}
	return delay
}

func (api *mqttAPI) Close() {
	api.closeOnce.Do(func() {
		close(api.done)
		api.wg.Wait()
		if api.client.IsConnectionOpen() {
			if err := waitMQTT(api.client.Publish(api.lwt_topic, 1, true, "offline"), api.timeout); err != nil {
				log.Printf("MQTT offline status: %v", err)
			}
		}
		api.client.Disconnect(0)
	})
}

func NewMqttAPI(broker, user, password string, timeout, resyncInterval time.Duration,
	lwtTopic, stateTopic, setTopic string) (*mqttAPI, error) {
	if timeout <= 0 || resyncInterval <= 0 {
		return nil, errors.New("MQTT timeout and resync interval must be positive")
	}
	api := &mqttAPI{
		lwt_topic: lwtTopic, state_topic: stateTopic, set_topic: setTopic,
		timeout: timeout, resyncInterval: resyncInterval,
		retained: map[string]string{lwtTopic: "offline"}, pending: make(map[string]bool),
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		commands: make(chan mqttCommand, 100), cbReady: make(chan struct{}), initial: make(chan error, 1),
	}
	options := mqtt.NewClientOptions()
	options.Username, options.Password = user, password
	options.AddBroker(broker)
	options.ClientID = "modbus2mqtt_" + strconv.Itoa(os.Getpid())
	options.Order = true
	options.AutoReconnect = true
	options.ConnectTimeout = timeout
	options.WriteTimeout = timeout
	options.SetWill(lwtTopic, "offline", 1, true)
	options.OnConnect = api.onConnect
	options.OnConnectionLost = func(_ mqtt.Client, err error) {
		api.mu.Lock()
		if !api.client.IsConnectionOpen() {
			api.connected = false
		}
		api.mu.Unlock()
		log.Printf("MQTT connection lost: %v", err)
		api.signal()
	}
	api.client = mqtt.NewClient(options)
	api.wg.Add(2)
	go api.publishLoop()
	go api.commandLoop()
	if err := waitMQTT(api.client.Connect(), timeout); err != nil {
		api.Close()
		return nil, fmt.Errorf("MQTT connect: %w", err)
	}
	select {
	case err := <-api.initial:
		if err == nil {
			return api, nil
		}
		api.Close()
		return nil, fmt.Errorf("MQTT subscribe: %w", err)
	case <-time.After(timeout):
		api.Close()
		return nil, errMqttTimeout
	}
}
