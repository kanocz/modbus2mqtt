package main

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simonvetter/modbus"
)

var (
	errWritableNotFound    = errors.New("writible register not found")
	errWritableInvalidType = errors.New("writible value doesn't suit register type")
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
}

type modbusAPI struct {
	registers      []configStructRegisters
	client         *modbus.ModbusClient
	cb_u16         cbU16
	cb_str         cbStr
	cb_bool        cbBool
	writables      map[string]modbusWritable
	mu             sync.Mutex
	oldValues_u16  map[string]uint16
	oldValues_str  map[string]string
	oldValues_bool map[string]bool
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

func NewModbusAPI(config configStruct, URL string, timeout time.Duration, scan time.Duration,
	CBU16 cbU16, CBStr cbStr, CBBool cbBool, CBRaw cbRaw) (*modbusAPI, error) {

	client, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL:     *modbusEP,
		Timeout: *modbusTimeout,
	})

	if err != nil {
		return nil, err
	}

	err = client.Open()
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
	}

	for _, reg := range mapi.registers {
		if reg.Type != "hold" && reg.Type != "coils" {
			continue
		}
		if reg.Type == "hold" {
			for _, data := range reg.Data {
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

	go mapi.loop(scan)

	// create mqtt autodiscovery entries
	if config.MQTT.HATopic != "" {

		haAD := newAutoDiscovery(config.MQTT.HATopic, config.MQTT.HAPrefix,
			config.MQTT.State_topic, config.MQTT.Set_topic,
			config.MQTT.HAdevID, config.MQTT.HAdevName, config.MQTT.HAManu, config.MQTT.HAModel)

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
						data.HAClass, data.HAType,
						data.Values, data.ValueX10)
					CBRaw(topic, payload)

				}

			}
		}
	}

	return mapi, nil
}

func (mapi *modbusAPI) Set(name string, value string) error {

	r, ex := mapi.writables[name]
	if !ex {
		return errWritableNotFound
	}

	switch r.Type {
	case "bit":
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
		return mapi.SetBit(name, v)
	case "bits":
		if r.X10 {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return err
			}
			return mapi.SetBits(name, uint16(v*10))
		}
		v, err := strconv.ParseUint(value, 10, 16)
		if err != nil {
			return err
		}
		return mapi.SetBits(name, uint16(v))
	case "coil":
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
		return mapi.SetCoil(name, v)
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

func (mapi *modbusAPI) SetCoil(name string, value bool) error {
	mapi.mu.Lock()
	defer mapi.mu.Unlock()

	r, ex := mapi.writables[name]
	if !ex {
		return errWritableNotFound
	}

	err := mapi.client.WriteCoil(r.Reg, value)
	if err != nil {
		log.Panic("unable to write modbus coil: ", err)
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

	reg16, err := mapi.client.ReadRegister(r.Reg, modbus.HOLDING_REGISTER)
	if err != nil {
		log.Panic("unable to read modbus holding reg: ", err)
	}
	orig := reg16

	if value {
		reg16 = SetBit(reg16, r.Pos)
	} else {
		reg16 = ClearBit(reg16, r.Pos)
	}

	if reg16 != orig {
		err = mapi.client.WriteRegisters(r.Reg, []uint16{reg16})
		if err != nil {
			log.Panic("unable to write modbus reg: ", err)
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

	if r.Pos == 0 && r.Width == 16 {
		err := mapi.client.WriteRegisters(r.Reg, []uint16{value})
		if err != nil {
			log.Panic("unable to write modbus reg: ", err)
		}
		return nil
	}

	reg16, err := mapi.client.ReadRegister(r.Reg, modbus.HOLDING_REGISTER)
	if err != nil {
		log.Panic("unable to read modbus holding reg: ", err)
	}
	orig := reg16

	reg16 = SetXBit(reg16, value, r.Pos, r.Width)

	if reg16 != orig {
		err = mapi.client.WriteRegisters(r.Reg, []uint16{reg16})
		if err != nil {
			log.Panic("unable to write modbus reg: ", err)
		}
	}

	return nil
}

// resend all values via callbacks on the next loop
func (mapi *modbusAPI) Reload() {
	mapi.mu.Lock()
	defer mapi.mu.Unlock()

	mapi.oldValues_bool = map[string]bool{}
	mapi.oldValues_str = map[string]string{}
	mapi.oldValues_u16 = map[string]uint16{}
}

func (mapi *modbusAPI) loop(scan time.Duration) {

	for {
		func() {
			mapi.mu.Lock()
			defer mapi.mu.Unlock()

			time.Sleep(*modbusPause)

			var err error
			for regID, reg := range config.Registers {
				var reg16 uint16

				switch reg.Type {
				case "hold":
					reg16, err = mapi.client.ReadRegister(reg.Reg, modbus.HOLDING_REGISTER)
					if err == modbus.ErrRequestTimedOut {
						log.Println("modbus timeout")
						mapi.client.Close()
						err := mapi.client.Open()
						if err != nil {
							log.Panic("unable to open modbus client: ", err)
						}
						return
					}
					if err != nil {
						log.Panic("unable to read modbus holding reg: ", err)
					}
				case "input":
					reg16, err = mapi.client.ReadRegister(reg.Reg, modbus.INPUT_REGISTER)
					if err == modbus.ErrRequestTimedOut {
						log.Println("modbus timeout")
						mapi.client.Close()
						err := mapi.client.Open()
						if err != nil {
							log.Panic("unable to open modbus client: ", err)
						}
						return
					}
					if err != nil {
						log.Panic("unable to read modbus input reg: ", err)
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

							if !data.ValueX10 {
								if mapi.cb_u16 != nil {
									mapi.cb_u16(data.Name, v)
								}
							} else {
								if mapi.cb_str != nil {
									mapi.cb_str(data.Name, fmt.Sprintf("%d.%d", v/10, v%10))
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
							vls, err := mapi.client.ReadCoils(uint16(data.Pos), uint16(data.maxPos-data.Pos+1))
							if err == modbus.ErrRequestTimedOut {
								log.Println("modbus timeout")
								mapi.client.Close()
								err := mapi.client.Open()
								if err != nil {
									log.Panic("unable to open modbus client: ", err)
								}
								continue
							}
							if err != nil {
								log.Panic("unable to read coils: ", err)
							}
							for i := uint16(data.Pos); i <= uint16(data.maxPos); i++ {
								if o, e := mapi.oldValues_bool[normalizeName(data.Values[i])]; o != vls[i-uint16(data.Pos)] || !e {
									mapi.oldValues_bool[normalizeName(data.Values[i])] = vls[i-uint16(data.Pos)]
									mapi.cb_bool(normalizeName(data.Values[i]), vls[i-uint16(data.Pos)])
								}
							}
						}

					}

					// update last time this register was updated
					config.Registers[regID].Data[dataID].last = time.Now()
				}

			}
		}()
		time.Sleep(scan)
	}
}
