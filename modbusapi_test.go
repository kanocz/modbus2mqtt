package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/simonvetter/modbus"
)

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "lowercase", input: "hello", expected: "hello"},
		{name: "uppercase", input: "WORLD", expected: "world"},
		{name: "mixed case", input: "HeLLoWoRLD", expected: "helloworld"},
		{name: "numbers", input: "12345", expected: "12345"},
		{name: "special characters", input: "!@#$%^&*()", expected: "_"},
		{name: "mixed characters", input: "Hello123!", expected: "hello123_"},
		{name: "real example", input: "Date-time set: Set command", expected: "date_time_set_set_command"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeName(tt.input)
			if got != tt.expected {
				t.Errorf("normalizeName(%s) = %s, want %s", tt.input, got, tt.expected)
			}
		})
	}
}

func listenRTUTest(t *testing.T, addr string) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { listener.Close() })
	return listener
}

func serveRTUTest(t *testing.T, listener net.Listener, handle func(net.Conn) error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			err = handle(conn)
		}
		done <- err
	}()
	t.Cleanup(func() {
		if err := <-done; err != nil {
			t.Errorf("RTU gateway: %v", err)
		}
	})
}

func readRTUTestRequest(conn net.Conn) ([]byte, error) {
	request := make([]byte, 7)
	if _, err := io.ReadFull(conn, request); err != nil {
		return nil, err
	}
	remaining := 1
	if request[1] == 16 { // multiple-register write: data followed by CRC
		remaining = int(request[6]) + 2
	}
	tail := make([]byte, remaining)
	_, err := io.ReadFull(conn, tail)
	return append(request, tail...), err
}

func rtuTestFrame(payload []byte) []byte {
	crc := uint16(0xffff)
	for _, b := range payload {
		crc ^= uint16(b)
		for bit := 0; bit < 8; bit++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xa001
			} else {
				crc >>= 1
			}
		}
	}
	return append(payload, byte(crc), byte(crc>>8))
}

func newRTUTestAPI(t *testing.T, listener net.Listener) *modbusAPI {
	t.Helper()
	client, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL:     "rtuovertcp://" + listener.Addr().String(),
		Timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return &modbusAPI{
		client:         client,
		oldValues_u16:  map[string]uint16{},
		oldValues_str:  map[string]string{},
		oldValues_bool: map[string]bool{},
	}
}

func TestRTUOverTCPPollRecovers(t *testing.T) {
	for _, kind := range []string{"hold", "input", "coils"} {
		for _, fault := range []string{"timeout", "eof", "crc"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				listener := listenRTUTest(t, "127.0.0.1:0")
				addr := listener.Addr().String()
				serveRTUTest(t, listener, func(conn net.Conn) error {
					request, err := readRTUTestRequest(conn)
					if err != nil || fault == "eof" {
						return err
					}
					if fault == "crc" {
						response := rtuTestFrame([]byte{request[0], request[1], 2, 0, 42})
						response[len(response)-1] ^= 1
						if _, err := conn.Write(response); err != nil {
							return err
						}
					}
					_, err = io.Copy(io.Discard, conn) // leave the socket open until the client closes it
					return err
				})
				mapi := newRTUTestAPI(t, listener)
				if fault != "timeout" {
					mapi.readAttempts = 3
				}
				data := configStructRegistersData{Name: "value", Type: "bits", Width: 16}
				if kind == "coils" {
					data.Type = "coils"
					data.Values = map[uint16]string{0: "value"}
				}
				mapi.registers = []configStructRegisters{{Type: kind, Data: []configStructRegistersData{data}}}
				published := 0
				mapi.cb_u16 = func(name string, value uint16) {
					published++
					if name != "value" || value != 42 {
						t.Errorf("unexpected value %s=%d", name, value)
					}
				}
				mapi.cb_bool = func(name string, value bool) {
					published++
					if name != "value" || !value {
						t.Errorf("unexpected value %s=%v", name, value)
					}
				}

				wantErr := map[string]error{"timeout": modbus.ErrRequestTimedOut, "eof": io.EOF, "crc": modbus.ErrBadCRC}[fault]
				if err := mapi.poll(); !errors.Is(err, wantErr) {
					t.Fatalf("poll error = %v, want %v", err, wantErr)
				}
				if !mapi.reconnect || published != 0 || !mapi.registers[0].Data[0].last.IsZero() {
					t.Fatal("failed poll must schedule reconnect without publishing or marking data updated")
				}

				listener.Close()
				for attempt := 0; attempt < 2; attempt++ {
					if err := mapi.poll(); err == nil || !mapi.reconnect {
						t.Fatalf("unavailable gateway: error = %v, reconnect = %v", err, mapi.reconnect)
					}
				}

				listener = listenRTUTest(t, addr)
				serveRTUTest(t, listener, func(conn net.Conn) error {
					for i := 0; i < 2; i++ {
						request, err := readRTUTestRequest(conn)
						if err != nil {
							return err
						}
						payload := []byte{request[0], request[1], 2, 0, 42}
						if kind == "coils" {
							payload = []byte{request[0], request[1], 1, 1}
						}
						if _, err := conn.Write(rtuTestFrame(payload)); err != nil {
							return err
						}
					}
					return nil
				})
				for i := 0; i < 2; i++ {
					if err := mapi.poll(); err != nil {
						t.Fatal(err)
					}
				}
				if mapi.reconnect || published != 1 || mapi.registers[0].Data[0].last.IsZero() {
					t.Fatalf("recovery failed: reconnect = %v, published = %d", mapi.reconnect, published)
				}
			})
		}
	}
}

func TestRTUOverTCPSetTimeout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		register  modbusWritable
		readFirst bool
	}{
		{"coil", modbusWritable{Type: "coil"}, false},
		{"bit/read", modbusWritable{Type: "bit"}, false},
		{"bit/write", modbusWritable{Type: "bit"}, true},
		{"bits/full", modbusWritable{Type: "bits", Width: 16}, false},
		{"bits/read", modbusWritable{Type: "bits", Width: 2}, false},
		{"bits/write", modbusWritable{Type: "bits", Width: 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := listenRTUTest(t, "127.0.0.1:0")
			serveRTUTest(t, listener, func(conn net.Conn) error {
				request, err := readRTUTestRequest(conn)
				if err != nil {
					return err
				}
				if tc.readFirst {
					if _, err := conn.Write(rtuTestFrame([]byte{request[0], request[1], 2, 0, 0})); err != nil {
						return err
					}
					if _, err := readRTUTestRequest(conn); err != nil {
						return err
					}
				}
				n, err := io.Copy(io.Discard, conn)
				if n != 0 {
					return fmt.Errorf("timed-out command was retried (%d bytes)", n)
				}
				return err
			})
			mapi := newRTUTestAPI(t, listener)
			if tc.readFirst || tc.register.Type == "coil" || tc.register.Width == 16 {
				mapi.readAttempts = 3
			}
			mapi.writables = map[string]modbusWritable{"value": tc.register}
			if err := mapi.Set("value", "1"); !errors.Is(err, modbus.ErrRequestTimedOut) {
				t.Fatalf("Set error = %v, want timeout", err)
			}
			if !mapi.reconnect {
				t.Fatal("write failure did not schedule reconnect")
			}
			if err := mapi.Set("value", "1"); !errors.Is(err, errModbusDisconnected) {
				t.Fatalf("Set while disconnected = %v", err)
			}
		})
	}
}

func TestRTUOverTCPPauseBetweenReads(t *testing.T) {
	listener := listenRTUTest(t, "127.0.0.1:0")
	serveRTUTest(t, listener, func(conn net.Conn) error {
		var replied time.Time
		for i := 0; i < 2; i++ {
			request, err := readRTUTestRequest(conn)
			if err != nil {
				return err
			}
			if i > 0 && time.Since(replied) < *modbusPause {
				return fmt.Errorf("pause between requests = %s, want at least %s", time.Since(replied), *modbusPause)
			}
			replied = time.Now()
			if _, err := conn.Write(rtuTestFrame([]byte{request[0], request[1], 2, 0, 0})); err != nil {
				return err
			}
		}
		return nil
	})
	mapi := newRTUTestAPI(t, listener)
	mapi.registers = []configStructRegisters{
		{Type: "hold", Reg: 0, Data: []configStructRegistersData{{Name: "one", Type: "bits", Width: 16}}},
		{Type: "input", Reg: 1, Data: []configStructRegistersData{{Name: "two", Type: "bits", Width: 16}}},
	}
	if err := mapi.poll(); err != nil {
		t.Fatal(err)
	}
}

func TestNumericValueValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reg     modbusWritable
		value   string
		want    uint16
		invalid bool
	}{
		{"four bit max", modbusWritable{Width: 4}, "15", 15, false},
		{"four bit overflow", modbusWritable{Width: 4}, "16", 0, true},
		{"unsigned negative", modbusWritable{Width: 16, X10: true}, "-1", 0, true},
		{"unsigned scaled max", modbusWritable{Width: 16, X10: true}, "6553.5", 65535, false},
		{"unsigned scaled overflow", modbusWritable{Width: 16, X10: true}, "6553.6", 0, true},
		{"signed scaled max", modbusWritable{Width: 16, Signed: true, X10: true}, "3276.7", 32767, false},
		{"signed scaled min", modbusWritable{Width: 16, Signed: true, X10: true}, "-3276.8", 32768, false},
		{"signed scaled overflow", modbusWritable{Width: 16, Signed: true, X10: true}, "4000", 0, true},
		{"signed narrow min", modbusWritable{Width: 8, Signed: true}, "-128", 65408, false},
		{"signed narrow overflow", modbusWritable{Width: 8, Signed: true}, "128", 0, true},
		{"decimal precision", modbusWritable{Width: 16, X10: true}, "1.11", 0, true},
		{"decimal tenth", modbusWritable{Width: 16, X10: true}, "1.1", 11, false},
		{"nan", modbusWritable{Width: 16, X10: true}, "NaN", 0, true},
		{"infinity", modbusWritable{Width: 16, X10: true}, "+Inf", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.reg.parseValue(tc.value)
			if tc.invalid {
				if err == nil {
					t.Fatalf("accepted %s as %d", tc.value, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}
	api := modbusAPI{writables: map[string]modbusWritable{"field": {Type: "bits", Width: 4}}}
	if err := api.SetBits("field", 16); !errors.Is(err, errValueOutOfRange) {
		t.Fatalf("direct write validation: %v", err)
	}
	api.writables["enum"] = modbusWritable{Type: "bits_const", Width: 2, RValues: map[string]uint16{"off": 0, "on": 1}}
	if err := api.Set("enum", "2"); !errors.Is(err, errValueOutOfRange) {
		t.Fatalf("unknown enum accepted: %v", err)
	}
}

func TestOpenModbusWithRetry(t *testing.T) {
	if *modbusStartupAttempts != 3 {
		t.Fatal("default startup attempts must be three")
	}
	firstErr, lastErr := errors.New("first connection failure"), errors.New("last connection failure")
	for _, tc := range []struct {
		name                            string
		attempts, succeedsAt, wantCalls int
	}{
		{"first attempt succeeds", 3, 1, 1},
		{"second attempt succeeds", 3, 2, 2},
		{"last attempt succeeds", 3, 3, 3},
		{"attempts exhausted", 3, 0, 3},
		{"single attempt", 1, 0, 1},
		{"custom limit", 5, 0, 5},
		{"zero attempts", 0, 0, 0},
		{"negative attempts", -1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			pause := 5 * time.Millisecond
			var lastCall time.Time
			err := openModbusWithRetry(func() error {
				if !lastCall.IsZero() && time.Since(lastCall) < pause {
					t.Error("connection retried before the configured pause")
				}
				lastCall = time.Now()
				calls++
				if calls == tc.succeedsAt {
					return nil
				}
				if calls == tc.attempts {
					return lastErr
				}
				return firstErr
			}, tc.attempts, pause)
			if calls != tc.wantCalls {
				t.Fatalf("made %d connection attempts, want %d", calls, tc.wantCalls)
			}
			if tc.succeedsAt > 0 {
				if err != nil {
					t.Fatalf("successful connection returned %v", err)
				}
			} else if err == nil {
				t.Fatal("expected startup error")
			} else if tc.attempts > 0 && !errors.Is(err, lastErr) {
				t.Fatalf("final connection error was lost: %v", err)
			}
		})
	}
}

func TestConstructorReadOnlyDiscoveryAndStartup(t *testing.T) {
	listener := listenRTUTest(t, "127.0.0.1:0")
	serveRTUTest(t, listener, func(conn net.Conn) error {
		request, err := readRTUTestRequest(conn)
		if err != nil {
			return err
		}
		if count := binary.BigEndian.Uint16(request[4:6]); count != 2 {
			return fmt.Errorf("expected one batch of two registers, got %d", count)
		}
		_, err = conn.Write(rtuTestFrame([]byte{1, 3, 4, 0, 1, 0, 1}))
		return err
	})
	cfg := configStruct{Registers: []configStructRegisters{
		{Type: "hold", Reg: 1, Data: []configStructRegistersData{{Name: "readonly", Type: "bits", Width: 16, HAUnit: "kWh", HAType: "measurement"}}},
		{Type: "hold", Reg: 2, Data: []configStructRegistersData{{Name: "writable", Type: "bits", Width: 16, Writable: true}}},
	}}
	cfg.MQTT.HATopic = "homeassistant"
	cfg.MQTT.Lwt_topic = "availability"
	publications := make(chan struct{}, 2)
	discovery := 0
	api, err := NewModbusAPI(cfg, "rtuovertcp://"+listener.Addr().String(), time.Second, time.Hour, 3,
		func(string, uint16) { publications <- struct{}{} }, nil, nil, func(_ string, payload string) {
			var entry haAutoDiscoveryEntry
			if err := json.Unmarshal([]byte(payload), &entry); err != nil {
				t.Error(err)
			}
			if entry.AvailabilityTopic != "availability" {
				t.Errorf("missing availability: %s", payload)
			}
			if discovery == 0 {
				if entry.UnitOfMeasuremet != "kWh" || entry.CommandTopic != "" {
					t.Errorf("incorrect readonly discovery: %s", payload)
				}
				time.Sleep(150 * time.Millisecond)
			}
			discovery++
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	if _, ok := api.writables["readonly"]; ok {
		t.Fatal("readonly field can be written")
	}
	if err := api.Set("readonly", "2"); !errors.Is(err, errWritableNotFound) {
		t.Fatalf("readonly write: %v", err)
	}
	if _, ok := api.writables["writable"]; !ok {
		t.Fatal("writable field was excluded")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-publications:
		case <-time.After(time.Second):
			t.Fatal("poll did not start")
		}
	}
	if discovery != 2 {
		t.Fatalf("discovery count=%d", discovery)
	}
}

func TestCoilAddresses(t *testing.T) {
	for _, values := range []map[uint16]string{{0: "zero", 1: "one"}, {1: "filters", 4: "night", 5: "intensive"}, {65535: "last"}} {
		t.Run(fmt.Sprint(values), func(t *testing.T) {
			cfg := configStruct{Registers: []configStructRegisters{{Type: "coils", Data: []configStructRegistersData{{Type: "coils", Values: values}}}}}
			if err := cfg.prepareRegisters(); err != nil {
				t.Fatal(err)
			}
			listener := listenRTUTest(t, "127.0.0.1:0")
			serveRTUTest(t, listener, func(conn net.Conn) error {
				if _, err := readRTUTestRequest(conn); err != nil {
					return err
				}
				_, err := conn.Write(rtuTestFrame([]byte{1, 1, 1, 255}))
				return err
			})
			api := newRTUTestAPI(t, listener)
			api.registers = cfg.Registers
			got := map[string]bool{}
			api.cb_bool = func(name string, value bool) { got[name] = value }
			if err := api.poll(); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(values) {
				t.Fatalf("unexpected publications: %v", got)
			}
			for _, name := range values {
				if !got[name] {
					t.Errorf("missing coil %s", name)
				}
			}
		})
	}
}

func TestExceptionDoesNotStopOtherRegisters(t *testing.T) {
	for _, kind := range []string{"hold", "input", "coils"} {
		t.Run(kind, func(t *testing.T) {
			listener := listenRTUTest(t, "127.0.0.1:0")
			serveRTUTest(t, listener, func(conn net.Conn) error {
				request, err := readRTUTestRequest(conn)
				if err != nil {
					return err
				}
				if _, err := conn.Write(rtuTestFrame([]byte{1, request[1] | 0x80, 2})); err != nil {
					return err
				}
				request, err = readRTUTestRequest(conn)
				if err != nil {
					return err
				}
				if binary.BigEndian.Uint16(request[2:4]) != 2 {
					return fmt.Errorf("wrong next register: %v", request)
				}
				_, err = conn.Write(rtuTestFrame([]byte{1, 3, 2, 0, 42}))
				return err
			})
			api := newRTUTestAPI(t, listener)
			data := configStructRegistersData{Name: "bad", Type: "bits", Width: 16}
			api.readAttempts = 3
			if kind == "coils" {
				data = configStructRegistersData{Type: "coils", Values: map[uint16]string{0: "bad"}}
			}
			api.registers = []configStructRegisters{{Type: kind, Reg: 1, Data: []configStructRegistersData{data}}, {Type: "hold", Reg: 2, Data: []configStructRegistersData{{Name: "good", Type: "bits", Width: 16}}}}
			api.cb_bool = func(string, bool) { t.Error("published failed coil") }
			got := map[string]uint16{}
			api.cb_u16 = func(name string, v uint16) { got[name] = v }
			if err := api.poll(); err != nil {
				t.Fatal(err)
			}
			if api.reconnect || !reflect.DeepEqual(got, map[string]uint16{"good": 42}) {
				t.Fatalf("reconnect=%v, values=%v", api.reconnect, got)
			}
			if !api.registers[0].Data[0].last.IsZero() {
				t.Fatal("failed field marked fresh")
			}
		})
	}
}

func TestDailyReadSchedule(t *testing.T) {
	listener := listenRTUTest(t, "127.0.0.1:0")
	serveRTUTest(t, listener, func(conn net.Conn) error {
		for _, addr := range []uint16{1, 2, 2, 1, 2} {
			request, err := readRTUTestRequest(conn)
			if err != nil {
				return err
			}
			if got := binary.BigEndian.Uint16(request[2:4]); got != addr {
				return fmt.Errorf("read %d, want %d", got, addr)
			}
			if _, err := conn.Write(rtuTestFrame([]byte{1, 3, 2, 0, 1})); err != nil {
				return err
			}
		}
		return nil
	})
	api := newRTUTestAPI(t, listener)
	api.registers = []configStructRegisters{
		{Type: "hold", Reg: 1, Data: []configStructRegistersData{{Name: "daily", Type: "bits", Width: 16, Daily: true}}},
		{Type: "hold", Reg: 2, Data: []configStructRegistersData{{Name: "normal", Type: "bits", Width: 16}}},
	}
	if err := api.poll(); err != nil {
		t.Fatal(err)
	}
	last := api.registers[0].Data[0].last
	if err := api.poll(); err != nil {
		t.Fatal(err)
	}
	if api.registers[0].Data[0].last != last {
		t.Fatal("daily register refreshed early")
	}
	api.registers[0].Data[0].last = time.Now().Add(-25 * time.Hour)
	if err := api.poll(); err != nil {
		t.Fatal(err)
	}
}

func TestSignedFieldReads(t *testing.T) {
	listener := listenRTUTest(t, "127.0.0.1:0")
	serveRTUTest(t, listener, func(conn net.Conn) error {
		for _, value := range []uint16{0x8000, 0x0080} {
			if _, err := readRTUTestRequest(conn); err != nil {
				return err
			}
			if _, err := conn.Write(rtuTestFrame([]byte{1, 3, 2, byte(value >> 8), byte(value)})); err != nil {
				return err
			}
		}
		return nil
	})
	api := newRTUTestAPI(t, listener)
	api.registers = []configStructRegisters{
		{Type: "hold", Data: []configStructRegistersData{{Name: "scaled", Type: "bits", Width: 16, Signed: true, ValueX10: true}}},
		{Type: "hold", Data: []configStructRegistersData{{Name: "narrow", Type: "bits", Width: 8, Signed: true}}},
	}
	got := map[string]string{}
	api.cb_str = func(name, value string) { got[name] = value }
	if err := api.poll(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]string{"scaled": "-3276.8", "narrow": "-128"}) {
		t.Fatalf("wrong signed values: %v", got)
	}
}
