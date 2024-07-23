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

type modbusWritable struct {
	Type    string
	Pos     int
	Width   int
	RValues map[string]uint16
	Reg     uint16
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

func NewModbusAPI(config configStruct, URL string, timeout time.Duration, scan time.Duration, CBU16 cbU16, CBStr cbStr, CBBool cbBool) (*modbusAPI, error) {

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

	return mapi, nil
}

func (mapi *modbusAPI) Set(name string, value string) error {

	r, ex := mapi.writables[name]
	if !ex {
		return errWritableNotFound
	}

	switch r.Type {
	case "bit":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		return mapi.SetBit(name, v)
	case "bits":
		v, err := strconv.ParseUint(value, 10, 16)
		if err != nil {
			return err
		}
		return mapi.SetBits(name, uint16(v))
	case "coil":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		return mapi.SetCoil(name, v)
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

	if r.Type != "bits" {
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

	for range time.NewTicker(scan).C {
		func() {
			mapi.mu.Lock()
			defer mapi.mu.Unlock()

			var err error
			for _, reg := range config.Registers {
				var reg16 uint16

				switch reg.Type {
				case "hold":
					reg16, err = mapi.client.ReadRegister(reg.Reg, modbus.HOLDING_REGISTER)
					if err != nil {
						log.Panic("unable to read modbus holding reg: ", err)
					}
				case "input":
					reg16, err = mapi.client.ReadRegister(reg.Reg, modbus.INPUT_REGISTER)
					if err != nil {
						log.Panic("unable to read modbus input reg: ", err)
					}
				}

				for _, data := range reg.Data {
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
				}
			}
		}()
	}
}
