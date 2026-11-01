package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/simonvetter/modbus"
)

func TestReadRetries(t *testing.T) {
	for _, kind := range []string{"hold", "input", "coils"} {
		for _, succeedsAt := range []int{0, 2, 3} {
			t.Run(fmt.Sprintf("%s/success_%d", kind, succeedsAt), func(t *testing.T) {
				listener := listenRTUTest(t, "127.0.0.1:0")
				serveRTUTest(t, listener, func(conn net.Conn) error {
					var first []byte
					for attempt := 1; attempt <= 3; attempt++ {
						request, err := readRTUTestRequest(conn)
						if err != nil {
							return err
						}
						if first == nil {
							first = request
						} else if !bytes.Equal(first, request) {
							return fmt.Errorf("retry changed request: %x -> %x", first, request)
						}
						if attempt == succeedsAt {
							payload := []byte{1, request[1], 2, 0, 42}
							if kind == "coils" {
								payload = []byte{1, 1, 1, 1}
							}
							if _, err := conn.Write(rtuTestFrame(payload)); err != nil {
								return err
							}
							// The next poll must keep using this connection too.
							request, err = readRTUTestRequest(conn)
							if err != nil {
								return err
							}
							if !bytes.Equal(first, request) {
								return fmt.Errorf("next poll changed request: %x", request)
							}
							_, err = conn.Write(rtuTestFrame(payload))
							return err
						}
						if attempt == 2 {
							// A body timeout must also allow another request on this connection.
							if _, err := conn.Write([]byte{1, request[1], 2}); err != nil {
								return err
							}
						}
					}
					n, err := io.Copy(io.Discard, conn)
					if n != 0 {
						return fmt.Errorf("more than three attempts: %d extra bytes", n)
					}
					return err
				})
				api := newRTUTestAPI(t, listener)
				api.readAttempts = 3
				field := configStructRegistersData{Name: "value", Type: "bits", Width: 16}
				if kind == "coils" {
					field = configStructRegistersData{Type: "coils", Values: map[uint16]string{0: "value"}}
				}
				api.registers = []configStructRegisters{{Type: kind, Data: []configStructRegistersData{field}}}
				published := 0
				api.cb_u16 = func(_ string, value uint16) {
					published++
					if value != 42 {
						t.Errorf("wrong value %d", value)
					}
				}
				api.cb_bool = func(_ string, value bool) {
					published++
					if !value {
						t.Error("wrong coil value")
					}
				}
				err := api.poll()
				if succeedsAt == 0 {
					if !errors.Is(err, modbus.ErrRequestTimedOut) || !api.reconnect || published != 0 || !api.registers[0].Data[0].last.IsZero() {
						t.Fatalf("exhausted retries: error=%v reconnect=%v published=%d", err, api.reconnect, published)
					}
				} else if err != nil || api.reconnect || published != 1 {
					t.Fatalf("recovery: error=%v reconnect=%v published=%d", err, api.reconnect, published)
				}
				if succeedsAt != 0 {
					if err := api.poll(); err != nil || published != 1 {
						t.Fatalf("poll after recovery: error=%v published=%d", err, published)
					}
				}
			})
		}
	}
}

func TestReadModifyWriteRetriesOnlyRead(t *testing.T) {
	for _, kind := range []string{"bit", "bits"} {
		for _, writeTimeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/write_timeout_%v", kind, writeTimeout), func(t *testing.T) {
				listener := listenRTUTest(t, "127.0.0.1:0")
				serveRTUTest(t, listener, func(conn net.Conn) error {
					wantRead := rtuTestFrame([]byte{1, 3, 0, 10, 0, 1})
					for attempt := 1; attempt <= 3; attempt++ {
						request, err := readRTUTestRequest(conn)
						if err != nil {
							return err
						}
						if !bytes.Equal(request, wantRead) {
							return fmt.Errorf("unexpected read request: %x", request)
						}
					}
					if _, err := conn.Write(rtuTestFrame([]byte{1, 3, 2, 0, 0})); err != nil {
						return err
					}
					request, err := readRTUTestRequest(conn)
					if err != nil {
						return err
					}
					if !bytes.Equal(request, rtuTestFrame([]byte{1, 16, 0, 10, 0, 1, 2, 0, 1})) {
						return fmt.Errorf("unexpected write request: %x", request)
					}
					if !writeTimeout {
						if _, err := conn.Write(rtuTestFrame([]byte{1, 16, 0, 10, 0, 1})); err != nil {
							return err
						}
					}
					n, err := io.Copy(io.Discard, conn)
					if n != 0 {
						return fmt.Errorf("write was repeated: %d bytes", n)
					}
					return err
				})
				api := newRTUTestAPI(t, listener)
				api.readAttempts = 3
				api.writables = map[string]modbusWritable{"value": {Type: kind, Reg: 10, Width: 2}}
				err := api.Set("value", "1")
				if writeTimeout {
					if !errors.Is(err, modbus.ErrRequestTimedOut) || !api.reconnect {
						t.Fatalf("write timeout: error=%v reconnect=%v", err, api.reconnect)
					}
				} else if err != nil || api.reconnect {
					t.Fatalf("read/write recovery: error=%v reconnect=%v", err, api.reconnect)
				}
			})
		}
	}
}

type registerRead struct{ function, address, count uint16 }

func readTestRegister(kind string, address uint16) configStructRegisters {
	return configStructRegisters{Type: kind, Reg: address, Data: []configStructRegistersData{
		{Name: fmt.Sprintf("%s_%d", kind, address), Type: "bits", Width: 16},
	}}
}

func TestBatchedReads(t *testing.T) {
	fresh := time.Now()
	registers := []configStructRegisters{
		readTestRegister("hold", 10), readTestRegister("hold", 11), readTestRegister("hold", 12),
		readTestRegister("hold", 13), readTestRegister("input", 13), readTestRegister("input", 14),
		readTestRegister("input", 17), readTestRegister("input", 65535), readTestRegister("input", 0),
	}
	registers[0].Data[0].Daily = true
	registers[2].Data[0].Daily, registers[2].Data[0].last = true, fresh
	requests := []registerRead{
		{3, 10, 2}, {3, 13, 1}, {4, 13, 2}, {4, 17, 1}, {4, 65535, 1}, {4, 0, 1},
		// The next poll must split off the now-cached daily register at 10.
		{3, 11, 1}, {3, 13, 1}, {4, 13, 2}, {4, 17, 1}, {4, 65535, 1}, {4, 0, 1},
	}
	listener := listenRTUTest(t, "127.0.0.1:0")
	serveRegisterReads(t, listener, requests)
	api := newRTUTestAPI(t, listener)
	api.registers, api.batchSize = registers, 125
	got := map[string]uint16{}
	api.cb_u16 = func(name string, value uint16) { got[name] = value }
	for i := 0; i < 2; i++ {
		if err := api.poll(); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]uint16{}
	for _, reg := range registers {
		if reg.Type != "hold" || reg.Reg != 12 {
			want[reg.Data[0].Name] = reg.Reg
		}
	}
	if !reflect.DeepEqual(got, want) || api.registers[2].Data[0].last != fresh {
		t.Fatalf("wrong values or daily timestamp: %v", got)
	}
}

func serveRegisterReads(t *testing.T, listener net.Listener, requests []registerRead) {
	t.Helper()
	serveRTUTest(t, listener, func(conn net.Conn) error {
		for _, want := range requests {
			request, err := readRTUTestRequest(conn)
			if err != nil {
				return err
			}
			got := registerRead{uint16(request[1]), binary.BigEndian.Uint16(request[2:4]), binary.BigEndian.Uint16(request[4:6])}
			if got != want {
				return fmt.Errorf("request %+v, want %+v", got, want)
			}
			payload := []byte{1, byte(want.function), byte(2 * want.count)}
			for i := uint16(0); i < want.count; i++ {
				value := want.address + i
				payload = append(payload, byte(value>>8), byte(value))
			}
			if _, err := conn.Write(rtuTestFrame(payload)); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestBatchSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		size, count int
		requests    []registerRead
	}{
		{1, 3, []registerRead{{3, 1, 1}, {3, 2, 1}, {3, 3, 1}}},
		{2, 3, []registerRead{{3, 1, 2}, {3, 3, 1}}},
		{125, 126, []registerRead{{3, 1, 125}, {3, 126, 1}}},
	} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			listener := listenRTUTest(t, "127.0.0.1:0")
			serveRegisterReads(t, listener, tc.requests)
			api := newRTUTestAPI(t, listener)
			api.batchSize = tc.size
			for i := 1; i <= tc.count; i++ {
				api.registers = append(api.registers, readTestRegister("hold", uint16(i)))
			}
			published := 0
			api.cb_u16 = func(name string, value uint16) {
				published++
				if name != fmt.Sprintf("hold_%d", value) {
					t.Errorf("value assigned to wrong register: %s=%d", name, value)
				}
			}
			if err := api.poll(); err != nil || published != tc.count {
				t.Fatalf("batch read: error=%v published=%d", err, published)
			}
		})
	}
}

func TestBatchExceptionFallsBackToSingles(t *testing.T) {
	for _, exception := range []byte{2, 3} {
		t.Run(fmt.Sprint(exception), func(t *testing.T) {
			listener := listenRTUTest(t, "127.0.0.1:0")
			serveRTUTest(t, listener, func(conn net.Conn) error {
				for i, want := range []registerRead{{3, 1, 2}, {3, 1, 1}, {3, 2, 1}} {
					request, err := readRTUTestRequest(conn)
					if err != nil {
						return err
					}
					got := registerRead{uint16(request[1]), binary.BigEndian.Uint16(request[2:4]), binary.BigEndian.Uint16(request[4:6])}
					if got != want {
						return fmt.Errorf("request %+v, want %+v", got, want)
					}
					payload := []byte{1, 0x83, exception}
					if i == 2 {
						payload = []byte{1, 3, 2, 0, 42}
					}
					if _, err := conn.Write(rtuTestFrame(payload)); err != nil {
						return err
					}
				}
				return nil
			})
			api := newRTUTestAPI(t, listener)
			api.readAttempts, api.batchSize = 3, 125
			api.registers = []configStructRegisters{readTestRegister("hold", 1), readTestRegister("hold", 2)}
			got := map[string]uint16{}
			api.cb_u16 = func(name string, value uint16) { got[name] = value }
			if err := api.poll(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, map[string]uint16{"hold_2": 42}) || !api.registers[0].Data[0].last.IsZero() || api.reconnect {
				t.Fatalf("fallback failed: values=%v reconnect=%v", got, api.reconnect)
			}
		})
	}
}

func TestReadSettingsValidation(t *testing.T) {
	oldAttempts, oldSize := *modbusReadAttempts, *modbusBatchSize
	t.Cleanup(func() { *modbusReadAttempts, *modbusBatchSize = oldAttempts, oldSize })
	for _, settings := range [][2]int{{0, 1}, {-1, 1}, {3, 0}, {3, 126}} {
		*modbusReadAttempts, *modbusBatchSize = settings[0], settings[1]
		if _, err := NewModbusAPI(configStruct{}, "rtuovertcp://127.0.0.1:1", time.Second, time.Second, 1, nil, nil, nil, nil); err == nil {
			t.Fatalf("accepted invalid read settings %v", settings)
		}
	}
}
