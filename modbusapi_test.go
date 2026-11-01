package main

import (
	"errors"
	"fmt"
	"io"
	"net"
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
	mapi.registers = []configStructRegisters{{Type: "hold", Reg: 0}, {Type: "input", Reg: 1}}
	if err := mapi.poll(); err != nil {
		t.Fatal(err)
	}
}
