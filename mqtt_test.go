package main

import (
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

type mqttTestToken struct{ err error }

func (t mqttTestToken) Wait() bool                     { return true }
func (t mqttTestToken) WaitTimeout(time.Duration) bool { return true }
func (t mqttTestToken) Error() error                   { return t.err }
func (t mqttTestToken) Done() <-chan struct{}          { ch := make(chan struct{}); close(ch); return ch }

type mqttSent struct{ topic, payload string }
type mqttTestClient struct {
	mqtt.Client
	sent          []mqttSent
	subscriptions int
	err           error
	onPublish     func()
}

func (c *mqttTestClient) IsConnectionOpen() bool { return true }
func (c *mqttTestClient) Subscribe(string, byte, mqtt.MessageHandler) mqtt.Token {
	c.subscriptions++
	return mqttTestToken{c.err}
}
func (c *mqttTestClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	if qos != 1 || !retained {
		panic("publication must use retained QoS 1")
	}
	c.sent = append(c.sent, mqttSent{topic, payload.(string)})
	if c.onPublish != nil {
		c.onPublish()
	}
	return mqttTestToken{c.err}
}
func newMQTTTestAPI() *mqttAPI {
	return &mqttAPI{client: &mqttTestClient{}, timeout: time.Second, resyncInterval: 5 * time.Minute,
		lwt_topic: "availability", state_topic: "state/", set_topic: "set/", connected: true,
		retained: map[string]string{"availability": "online"}, pending: map[string]bool{},
		wake: make(chan struct{}, 1), done: make(chan struct{}), initial: make(chan error, 1),
		cbReady: make(chan struct{}), commands: make(chan mqttCommand, 100),
	}
}

func TestMQTTResyncThrottle(t *testing.T) {
	if *mqttResyncInterval != 5*time.Minute {
		t.Fatal("default resync interval must be five minutes")
	}
	api := newMQTTTestAPI()
	client := api.client.(*mqttTestClient)
	api.retained["state/daily"] = "42"
	api.retained["homeassistant/sensor/config"] = "discovery"
	api.lastResync = time.Now()
	api.onConnect(client)
	if delay := api.flush(); delay <= 0 || delay > 5*time.Minute {
		t.Fatalf("invalid deferred resync delay %s", delay)
	}
	if !reflect.DeepEqual(client.sent, []mqttSent{{"availability", "online"}}) {
		t.Fatalf("full resend happened early: %v", client.sent)
	}
	api.onConnect(client)
	api.Publish("daily", "43")
	api.flush()
	if !api.resyncRequested {
		t.Fatal("pending resync was lost")
	}
	for _, sent := range client.sent {
		if sent.topic == "homeassistant/sensor/config" {
			t.Fatal("discovery resent early")
		}
	}
	api.lastResync = time.Now().Add(-5 * time.Minute)
	client.sent = nil
	api.flush()
	if len(client.sent) != 3 || api.resyncRequested {
		t.Fatalf("full resync missing: %v", client.sent)
	}
	found := false
	for _, sent := range client.sent {
		if sent.topic == "state/daily" && sent.payload == "43" {
			found = true
		}
	}
	if !found {
		t.Fatal("resync used stale value")
	}
	client.sent = nil
	api.onConnect(client)
	api.flush()
	if len(client.sent) != 1 || client.subscriptions != 3 {
		t.Fatalf("resync not throttled or subscription missing: %v / %d", client.sent, client.subscriptions)
	}
}

func TestMQTTPublishFailureAndConcurrentUpdate(t *testing.T) {
	api := newMQTTTestAPI()
	client := api.client.(*mqttTestClient)
	api.Publish("value", "1")
	client.err = errors.New("send failed")
	if delay := api.flush(); delay != api.timeout || !api.pending["state/value"] {
		t.Fatal("failed publication was discarded")
	}
	client.err = nil
	client.onPublish = func() { client.onPublish = nil; api.Publish("value", "2") }
	api.flush()
	if !api.pending["state/value"] {
		t.Fatal("update arriving during publication was discarded")
	}
	api.flush()
	if len(api.pending) != 0 || client.sent[len(client.sent)-1].payload != "2" {
		t.Fatalf("latest value not delivered: %v", client.sent)
	}
}

func TestMQTTSubscribeRetry(t *testing.T) {
	api := newMQTTTestAPI()
	client := api.client.(*mqttTestClient)
	api.onConnect(client)
	client.err = errors.New("subscription failed")
	if delay := api.flush(); delay != api.timeout || !api.needsSubscribe {
		t.Fatal("subscription failure not retried")
	}
	client.err = nil
	api.flush()
	if api.needsSubscribe || client.subscriptions != 2 {
		t.Fatal("subscription did not recover")
	}
}

type mqttTestMessage string

func (m mqttTestMessage) Duplicate() bool   { return false }
func (m mqttTestMessage) Qos() byte         { return 1 }
func (m mqttTestMessage) Retained() bool    { return false }
func (m mqttTestMessage) Topic() string     { return "set/power" }
func (m mqttTestMessage) MessageID() uint16 { return 1 }
func (m mqttTestMessage) Payload() []byte   { return []byte(m) }
func (m mqttTestMessage) Ack()              {}

func TestMQTTCommandOrder(t *testing.T) {
	api := newMQTTTestAPI()
	started := make(chan struct{})
	release := make(chan struct{})
	got := make(chan string, 2)
	api.wg.Add(1)
	go api.commandLoop()
	api.msgHandler(nil, mqttTestMessage("on"))
	api.msgHandler(nil, mqttTestMessage("off"))
	api.SetCB(func(_ string, value string) {
		if value == "on" {
			close(started)
			<-release
		}
		got <- value
	})
	<-started
	select {
	case v := <-got:
		t.Fatalf("command overtook first one: %s", v)
	default:
	}
	close(release)
	if first, second := <-got, <-got; first != "on" || second != "off" {
		t.Fatalf("wrong order %s %s", first, second)
	}
	close(api.done)
	api.wg.Wait()
}

type mqttBrokerEvent struct {
	conn   net.Conn
	packet packets.ControlPacket
	at     time.Time
}
type mqttTestBroker struct {
	listener net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]bool
	events   chan mqttBrokerEvent
	done     chan struct{}
	wg       sync.WaitGroup
}

func newMQTTTestBroker(t *testing.T) *mqttTestBroker {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	broker := &mqttTestBroker{listener: listener, conns: map[net.Conn]bool{}, events: make(chan mqttBrokerEvent, 200), done: make(chan struct{})}
	broker.wg.Add(1)
	go func() {
		defer broker.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			broker.mu.Lock()
			broker.conns[conn] = true
			broker.mu.Unlock()
			broker.wg.Add(1)
			go broker.serve(conn)
		}
	}()
	t.Cleanup(func() {
		close(broker.done)
		listener.Close()
		broker.mu.Lock()
		for conn := range broker.conns {
			conn.Close()
		}
		broker.mu.Unlock()
		broker.wg.Wait()
	})
	return broker
}
func (b *mqttTestBroker) serve(conn net.Conn) {
	defer b.wg.Done()
	defer conn.Close()
	defer func() { b.mu.Lock(); delete(b.conns, conn); b.mu.Unlock() }()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	for {
		packet, err := packets.ReadPacket(conn)
		if err != nil {
			return
		}
		select {
		case b.events <- mqttBrokerEvent{conn, packet, time.Now()}:
		case <-b.done:
			return
		}
		var response packets.ControlPacket
		switch p := packet.(type) {
		case *packets.ConnectPacket:
			response = packets.NewControlPacket(packets.Connack)
		case *packets.SubscribePacket:
			ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
			ack.MessageID = p.MessageID
			ack.ReturnCodes = []byte{1}
			response = ack
		case *packets.PublishPacket:
			if p.Qos == 1 {
				ack := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
				ack.MessageID = p.MessageID
				response = ack
			}
		case *packets.PingreqPacket:
			response = packets.NewControlPacket(packets.Pingresp)
		case *packets.DisconnectPacket:
			return
		}
		if response != nil {
			if err := response.Write(conn); err != nil {
				return
			}
		}
	}
}
func (b *mqttTestBroker) wait(t *testing.T, match func(packets.ControlPacket) bool) mqttBrokerEvent {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-b.events:
			if match(event.packet) {
				return event
			}
		case <-deadline.C:
			t.Fatal("MQTT broker event timed out")
			return mqttBrokerEvent{}
		}
	}
}

func TestMQTTReconnectAndAvailability(t *testing.T) {
	broker := newMQTTTestBroker(t)
	interval := 300 * time.Millisecond
	api, err := NewMqttAPI("tcp://"+broker.listener.Addr().String(), "", "", time.Second, interval, "availability", "state/", "set/")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	isConnect := func(p packets.ControlPacket) bool { _, ok := p.(*packets.ConnectPacket); return ok }
	isSubscribe := func(p packets.ControlPacket) bool { _, ok := p.(*packets.SubscribePacket); return ok }
	first := broker.wait(t, isConnect)
	connect := first.packet.(*packets.ConnectPacket)
	if !connect.WillFlag || !connect.WillRetain || connect.WillQos != 1 || connect.WillTopic != "availability" || string(connect.WillMessage) != "offline" {
		t.Fatalf("incorrect Last Will: %v", connect)
	}
	broker.wait(t, isSubscribe)
	api.Online()
	api.RawPublish("homeassistant/sensor/config", "discovery")
	api.Publish("value", "42")
	want := map[string]string{"availability": "online", "homeassistant/sensor/config": "discovery", "state/value": "42"}
	seen := map[string]bool{}
	broker.wait(t, func(packet packets.ControlPacket) bool {
		if p, ok := packet.(*packets.PublishPacket); ok && string(p.Payload) == want[p.TopicName] {
			if !p.Retain {
				t.Error("publication is not retained")
			}
			seen[p.TopicName] = true
		}
		return len(seen) == 3
	})
	deadline := time.Now().Add(time.Second)
	for {
		api.mu.Lock()
		pending := len(api.pending)
		api.mu.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publications not acknowledged")
		}
		time.Sleep(time.Millisecond)
	}
	api.mu.Lock()
	api.lastResync = time.Now()
	base := api.lastResync
	api.mu.Unlock()
	first.conn.Close()
	second := broker.wait(t, isConnect)
	subscribed := broker.wait(t, isSubscribe).packet.(*packets.SubscribePacket)
	if !reflect.DeepEqual(subscribed.Topics, []string{"set/#"}) {
		t.Fatalf("wrong subscription: %v", subscribed.Topics)
	}
	commands := make(chan string, 2)
	api.SetCB(func(_ string, value string) { commands <- value })
	for _, value := range []string{"on", "off"} {
		p := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
		p.TopicName = "set/power"
		p.Payload = []byte(value)
		if err := p.Write(second.conn); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"on", "off"} {
		select {
		case got := <-commands:
			if got != want {
				t.Fatalf("command=%s want=%s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("commands missing after reconnect")
		}
	}
	seen = map[string]bool{}
	broker.wait(t, func(packet packets.ControlPacket) bool {
		if p, ok := packet.(*packets.PublishPacket); ok && (p.TopicName == "state/value" || p.TopicName == "homeassistant/sensor/config") {
			if time.Since(base) < interval {
				t.Error("full resync happened before minimum interval")
			}
			if string(p.Payload) != want[p.TopicName] {
				t.Error("wrong replay payload")
			}
			seen[p.TopicName] = true
		}
		return len(seen) == 2
	})
	api.Close()
	broker.wait(t, func(packet packets.ControlPacket) bool {
		p, ok := packet.(*packets.PublishPacket)
		return ok && p.TopicName == "availability" && string(p.Payload) == "offline" && p.Retain
	})
}

