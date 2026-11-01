package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simonvetter/modbus"
)

var (
	errWritableNotFound    = errors.New("writible register not found")
	errWritableInvalidType = errors.New("writible value doesn't suit register type")
	errModbusDisconnected  = errors.New("modbus disconnected; waiting for reconnect")
	errValueOutOfRange     = errors.New("value outside register range or precision")
)

type cbBool func(string, bool)
type cbU16 func(string, uint16)
type cbStr func(string, string)
type cbRaw func(string, string)

type modbusWritable struct {
	Type    string
	Pos     int
	Width   int
	RValues map[string]uint16
	Reg     uint16
	X10     bool
	Signed  bool
}

type modbusAPI struct {
	registers      []configStructRegisters
	client         *modbus.ModbusClient
	reconnect      bool
	cb_u16         cbU16
	cb_str         cbStr
	cb_bool        cbBool
	writables      map[string]modbusWritable
	mu             sync.Mutex
	oldValues_u16  map[string]uint16
	oldValues_str  map[string]string
	oldValues_bool map[string]bool
	done           chan struct{}
	wg             sync.WaitGroup
	closeOnce      sync.Once
}

func normalizeName(name string) string {
	result := strings.ReplaceAll(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		if r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, name), "__", "_")

	for strings.Contains(result, "__") {
		result = strings.ReplaceAll(result, "__", "_")
	}
	return result

}

func openModbusWithRetry(open func() error, attempts int, pause time.Duration) error {
	if attempts < 1 {
		return errors.New("modbus.startup-attempts must be at least 1")
	}
	for attempt := 1; ; attempt++ {
		err := open()
		if err == nil {
			return nil
		}
		if attempt == attempts {
			return fmt.Errorf("modbus startup connection failed after %d attempts: %w", attempts, err)
		}
		log.Printf("modbus startup connection attempt %d/%d failed: %v; retrying in %s", attempt, attempts, err, pause)
		time.Sleep(pause)
	}
}

func NewModbusAPI(config configStruct, URL string, timeout time.Duration, scan time.Duration, startupAttempts int,
	CBU16 cbU16, CBStr cbStr, CBBool cbBool, CBRaw cbRaw) (*modbusAPI, error) {
	if timeout <= 0 || scan <= 0 {
		return nil, errors.New("modbus timeout and scan must be positive")
	}
	if err := config.prepareRegisters(); err != nil {
		return nil, err
	}

	client, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL:     URL,
		Timeout: timeout,
	})

	if err != nil {
		return nil, err
	}

	err = openModbusWithRetry(client.Open, startupAttempts, scan)
	if err != nil {
		return nil, err
	}

	mapi := &modbusAPI{
		registers:      config.Registers,
		client:         client,
		cb_u16:         CBU16,
		cb_str:         CBStr,
		cb_bool:        CBBool,
		writables:      map[string]modbusWritable{},
		oldValues_u16:  map[string]uint16{},
		oldValues_str:  map[string]string{},
		oldValues_bool: map[string]bool{},
		done:           make(chan struct{}),
	}

	for _, reg := range mapi.registers {
		if reg.Type != "hold" && reg.Type != "coils" {
			continue
		}
		if reg.Type == "hold" {
			for _, data := range reg.Data {
				if !data.Writable {
					continue
				}
				var RValues map[string]uint16

				if len(data.Values) > 0 {
					RValues = map[string]uint16{}
					for k, v := range data.Values {
						RValues[v] = k
					}
				}

				mapi.writables[data.Name] = modbusWritable{
					Type:    data.Type,
					Pos:     data.Pos,
					Width:   data.Width,
					Reg:     reg.Reg,
					RValues: RValues,
					X10:     data.ValueX10,
					Signed:  data.Signed,
				}
			}
		} else { // coils
			for _, data := range reg.Data {
				if !data.Writable {
					continue
				}
				for k, v := range data.Values {
					mapi.writables[normalizeName(v)] = modbusWritable{
						Type: "coil",
						Reg:  k,
					}

				}
			}

		}
	}

	// create mqtt autodiscovery entries
	if config.MQTT.HATopic != "" && CBRaw != nil {

		haAD := newAutoDiscovery(config.MQTT.HATopic, config.MQTT.HAPrefix,
			config.MQTT.State_topic, config.MQTT.Set_topic,
			config.MQTT.HAdevID, config.MQTT.HAdevName, config.MQTT.HAManu, config.MQTT.HAModel)
		haAD.availabilityTopic = config.MQTT.Lwt_topic

		for _, reg := range mapi.registers {
			for _, data := range reg.Data {
				if reg.Type == "coils" {
					var topic, payload string
					for _, v := range data.Values {
						if !data.Writable {
							topic, payload = haAD.Get(v, data.origName, "binary_sensor", data.Writable, "", "", nil, false)
						} else {
							topic, payload = haAD.Get(v, data.origName, "switch", data.Writable, "", "", nil, false)
						}
						CBRaw(topic, payload)
					}
					continue
				}

				if reg.Type == "hold" || reg.Type == "input" {

					eType := ""

					switch {
					case data.Type == "bit" && data.Writable:
						eType = "switch"
					case data.Type == "bit" && !data.Writable:
						eType = "binary_sensor"
					case data.Type == "bits" && data.Writable:
						eType = "number"
					case data.Type == "bits" && !data.Writable:
						eType = "sensor"
					case data.Type == "bits_const" && !data.Writable:
						eType = "sensor"
					case data.Type == "bits_const" && data.Writable:
						eType = "select"
					}

					topic, payload := haAD.Get(data.Name, data.origName, eType, data.Writable,
						data.HAClass, data.HAUnit,
						data.Values, data.ValueX10)
					CBRaw(topic, payload)

				}

			}
		}
	}

	mapi.wg.Add(1)
	go func() {
		defer mapi.wg.Done()
		mapi.loop(scan)
	}()
	return mapi, nil
}

func (mapi *modbusAPI) Close() {
	mapi.closeOnce.Do(func() {
		close(mapi.done)
		mapi.wg.Wait()
		mapi.mu.Lock()
		defer mapi.mu.Unlock()
		mapi.client.Close()
	})
}

func (mapi *modbusAPI) Set(name string, value string) error {

	r, ex := mapi.writables[name]
	if !ex {
		return errWritableNotFound
	}

	switch r.Type {
	case "bit", "coil":
		var (
			v   bool
			err error
		)
		if strings.ToLower(value) == "on" { // special case for HA
			v = true
		} else if strings.ToLower(value) == "off" { // special case for HA
			v = false
		} else {
			v, err = strconv.ParseBool(value)
			if err != nil {
				return err
			}
		}
		if r.Type == "coil" {
			return mapi.SetCoil(name, v)
		}
		return mapi.SetBit(name, v)
	case "bits":
		v, err := r.parseValue(value)
		if err != nil {
			return err
		}
		return mapi.SetBits(name, v)
	case "bits_const":
		v, ex := r.RValues[value]
		if !ex {
			v64, err := strconv.ParseUint(value, 10, 16)
			if err != nil {
				return err
			}
			v = uint16(v64)
		}
		return mapi.SetBits(name, v)
	}

	return errWritableInvalidType
}

func (r modbusWritable) limits() (int64, int64) {
	if r.Signed {
		return -(1 << (r.Width - 1)), (1 << (r.Width - 1)) - 1
	}
	return 0, (1 << r.Width) - 1
}

func (r modbusWritable) parseValue(value string) (uint16, error) {
	if r.Width < 1 || r.Width > 16 {
		return 0, errWritableInvalidType
	}
	if r.X10 {
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, err
		}
		low, high := r.limits()
		scaled := math.Round(v * 10)
		if math.IsNaN(v) || math.IsInf(v, 0) || v < float64(low)/10 || v > float64(high)/10 || v != scaled/10 {
			return 0, errValueOutOfRange
		}
		return uint16(int64(scaled)), nil
	}
	if r.Signed {
		v, err := strconv.ParseInt(value, 10, r.Width)
		return uint16(v), err
	}
	v, err := strconv.ParseUint(value, 10, r.Width)
	return uint16(v), err
}

func (mapi *modbusAPI) SetCoil(name string, value bool) error {
	mapi.mu.Lock()
	defer mapi.mu.Unlock()

	r, ex := mapi.writables[name]
	if !ex {
		return errWritableNotFound
	}
	if r.Type != "coil" {
		return errWritableInvalidType
	}

	if mapi.reconnect {
		return errModbusDisconnected
	}
	time.Sleep(*modbusPause)
	err := mapi.client.WriteCoil(r.Reg, value)
	if err != nil {
		return mapi.connectionError("write coil", r.Reg, err)
	}

	return nil
}

func (mapi *modbusAPI) SetBit(name string, value bool) error {
	mapi.mu.Lock()
	defer mapi.mu.Unlock()

	r, ex := mapi.writables[name]
	if !ex {
		return errWritableNotFound
	}

	if r.Type != "bit" {
		return errWritableInvalidType
	}

	if mapi.reconnect {
		return errModbusDisconnected
	}
	time.Sleep(*modbusPause)
	reg16, err := mapi.client.ReadRegister(r.Reg, modbus.HOLDING_REGISTER)
	if err != nil {
		return mapi.connectionError("read holding register", r.Reg, err)
	}
	orig := reg16

	if value {
		reg16 = SetBit(reg16, r.Pos)
	} else {
		reg16 = ClearBit(reg16, r.Pos)
	}

	if reg16 != orig {
		time.Sleep(*modbusPause)
		err = mapi.client.WriteRegisters(r.Reg, []uint16{reg16})
		if err != nil {
			return mapi.connectionError("write register", r.Reg, err)
		}
	}

	return nil
}

func (mapi *modbusAPI) SetBits(name string, value uint16) error {
	mapi.mu.Lock()
	defer mapi.mu.Unlock()

	r, ex := mapi.writables[name]
	if !ex {
		return errWritableNotFound
	}

	if r.Type != "bits" && r.Type != "bits_const" {
		return errWritableInvalidType
	}
	if r.Width < 1 || r.Width > 16 {
		return errWritableInvalidType
	}
	low, high := r.limits()
	v := int64(value)
	if r.Signed {
		v = int64(int16(value))
	}
	if v < low || v > high {
		return errValueOutOfRange
	}
	if r.Type == "bits_const" {
		valid := false
		for _, allowed := range r.RValues {
			valid = valid || value == allowed
		}
		if !valid {
			return errValueOutOfRange
		}
	}

	if mapi.reconnect {
		return errModbusDisconnected
	}
	time.Sleep(*modbusPause)
	if r.Pos == 0 && r.Width == 16 {
		err := mapi.client.WriteRegisters(r.Reg, []uint16{value})
		if err != nil {
			return mapi.connectionError("write register", r.Reg, err)
		}
		return nil
	}

	reg16, err := mapi.client.ReadRegister(r.Reg, modbus.HOLDING_REGISTER)
	if err != nil {
		return mapi.connectionError("read holding register", r.Reg, err)
	}
	orig := reg16

	reg16 = SetXBit(reg16, value, r.Pos, r.Width)

	if reg16 != orig {
		time.Sleep(*modbusPause)
		err = mapi.client.WriteRegisters(r.Reg, []uint16{reg16})
		if err != nil {
			return mapi.connectionError("write register", r.Reg, err)
		}
	}

	return nil
}

func isModbusException(err error) bool {
	for _, exception := range []error{
		modbus.ErrIllegalFunction, modbus.ErrIllegalDataAddress, modbus.ErrIllegalDataValue,
		modbus.ErrServerDeviceFailure, modbus.ErrAcknowledge, modbus.ErrServerDeviceBusy,
		modbus.ErrMemoryParityError, modbus.ErrGWPathUnavailable, modbus.ErrGWTargetFailedToRespond,
	} {
		if errors.Is(err, exception) {
			return true
		}
	}
	return false
}

// Called with mu held; discard the connection so late RTU replies cannot reach the next request.
func (mapi *modbusAPI) connectionError(operation string, reg uint16, err error) error {
	if !isModbusException(err) {
		mapi.client.Close()
		mapi.reconnect = true
	}
	return fmt.Errorf("modbus %s at %d: %w", operation, reg, err)
}

func (mapi *modbusAPI) loop(scan time.Duration) {
	for {
		select {
		case <-mapi.done:
			return
		default:
		}
		if err := mapi.poll(); err != nil {
			log.Printf("%v; retrying in %s", err, scan)
		}
		timer := time.NewTimer(scan)
		select {
		case <-mapi.done:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (mapi *modbusAPI) poll() error {
	mapi.mu.Lock()
	defer mapi.mu.Unlock()

	if mapi.reconnect {
		if err := mapi.client.Open(); err != nil {
			return fmt.Errorf("unable to reconnect modbus client: %w", err)
		}
		mapi.reconnect = false
		log.Println("modbus reconnected")
	}

	for regID, reg := range mapi.registers {
		var reg16 uint16
		needsRead := false
		for _, data := range reg.Data {
			needsRead = needsRead || !data.Daily || time.Since(data.last) >= 24*time.Hour
		}
		if !needsRead {
			continue
		}
		if reg.Type == "hold" || reg.Type == "input" {
			kind := modbus.HOLDING_REGISTER
			if reg.Type == "input" {
				kind = modbus.INPUT_REGISTER
			}
			time.Sleep(*modbusPause)
			var err error
			reg16, err = mapi.client.ReadRegister(reg.Reg, kind)
			if err != nil {
				err = mapi.connectionError("read "+reg.Type+" register", reg.Reg, err)
				if isModbusException(err) {
					log.Print(err)
					continue
				}
				return err
			}
		}

		for dataID, data := range reg.Data {

			// check if we need to update this register
			if data.Daily && time.Since(data.last) < time.Hour*24 {
				continue
			}

			switch data.Type {
			case "bit":
				if mapi.cb_bool != nil {
					v := GetBit(reg16, data.Pos)
					if o, e := mapi.oldValues_bool[data.Name]; o != v || !e {
						mapi.oldValues_bool[data.Name] = v
						mapi.cb_bool(data.Name, v)
					}
				}
			case "bits":
				v := GetXBit(reg16, data.Pos, data.Width)
				if o, e := mapi.oldValues_u16[data.Name]; o != v || !e {
					mapi.oldValues_u16[data.Name] = v

					if !data.ValueX10 && !data.Signed && mapi.cb_u16 != nil {
						mapi.cb_u16(data.Name, v)
					} else if (data.ValueX10 || data.Signed) && mapi.cb_str != nil {
						n := int64(v)
						if data.Signed && v&(uint16(1)<<(data.Width-1)) != 0 {
							n -= 1 << data.Width
						}
						if data.ValueX10 {
							mapi.cb_str(data.Name, strconv.FormatFloat(float64(n)/10, 'f', 1, 64))
						} else {
							mapi.cb_str(data.Name, strconv.FormatInt(n, 10))
						}
					}

				}
			case "bits_const":
				if mapi.cb_str != nil {
					if s, ok := data.Values[GetXBit(reg16, data.Pos, data.Width)]; ok {
						if o, e := mapi.oldValues_str[data.Name]; o != s || !e {
							mapi.oldValues_str[data.Name] = s
							mapi.cb_str(data.Name, s)
						}
					}
				}
			case "coils":
				if mapi.cb_bool != nil {
					time.Sleep(*modbusPause)
					vls, err := mapi.client.ReadCoils(uint16(data.Pos), uint16(data.maxPos-data.Pos+1))
					if err != nil {
						err = mapi.connectionError("read coils", uint16(data.Pos), err)
						if isModbusException(err) {
							log.Print(err)
							continue
						}
						return err
					}
					for addr, label := range data.Values {
						name := normalizeName(label)
						value := vls[int(addr)-data.Pos]
						if old, exists := mapi.oldValues_bool[name]; old != value || !exists {
							mapi.oldValues_bool[name] = value
							mapi.cb_bool(name, value)
						}
					}
				}

			}

			// update last time this register was updated
			mapi.registers[regID].Data[dataID].last = time.Now()
		}

	}
	return nil
}
