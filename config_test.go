package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestExampleConfigs(t *testing.T) {
	oldConfig, oldPath := config, *configFile
	t.Cleanup(func() { config, *configFile = oldConfig, oldPath })
	for _, path := range []string{"configs/xvent.json", "configs/salda.json", "configs/salda_full.json"} {
		t.Run(path, func(t *testing.T) {
			*configFile = path
			if err := readConfig(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRegisterValidation(t *testing.T) {
	for _, tc := range []struct{ name, register string }{
		{"unknown register", `{"type":"holding","data":[{"name":"x","type":"bits","width":16}]}`},
		{"empty fields", `{"type":"hold"}`},
		{"unknown field", `{"type":"input","data":[{"name":"x","type":"bits_const ","width":16}]}`},
		{"negative bit", `{"type":"hold","data":[{"name":"x","type":"bit","position":-1}]}`},
		{"bit too high", `{"type":"hold","data":[{"name":"x","type":"bit","position":16}]}`},
		{"zero width", `{"type":"hold","data":[{"name":"x","type":"bits"}]}`},
		{"overflowing field", `{"type":"hold","data":[{"name":"x","type":"bits","position":12,"width":8}]}`},
		{"writable input", `{"type":"input","data":[{"name":"x","type":"bits","width":16,"writable":true}]}`},
		{"missing name", `{"type":"input","data":[{"type":"bits","width":16}]}`},
		{"duplicate name", `{"type":"hold","data":[{"name":"A-B","type":"bit"},{"name":"a b","type":"bit"}]}`},
		{"missing enum", `{"type":"hold","data":[{"name":"x","type":"bits_const","width":2}]}`},
		{"enum overflow", `{"type":"hold","data":[{"name":"x","type":"bits_const","width":2,"values":{"4":"bad"}}]}`},
		{"ambiguous writable enum", `{"type":"hold","data":[{"name":"x","type":"bits_const","width":2,"writable":true,"values":{"0":"same","1":"same"}}]}`},
		{"missing coils", `{"type":"coils","data":[{"type":"coils"}]}`},
		{"empty coil name", `{"type":"coils","data":[{"type":"coils","values":{"0":""}}]}`},
		{"excessive coil span", `{"type":"coils","data":[{"type":"coils","values":{"0":"first","2000":"last"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg configStruct
			if err := json.Unmarshal([]byte(`{"registers":[`+tc.register+`]}`), &cfg); err != nil {
				t.Fatal(err)
			}
			if err := cfg.prepareRegisters(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestCoilZeroAndConfigOwnership(t *testing.T) {
	original := configStruct{Registers: []configStructRegisters{{Type: "coils", Data: []configStructRegistersData{
		{Type: "coils", Values: map[uint16]string{0: "zero", 1: "one"}},
	}}}}
	for i := 0; i < 100; i++ {
		cfg := original
		if err := cfg.prepareRegisters(); err != nil {
			t.Fatal(err)
		}
		if data := cfg.Registers[0].Data[0]; data.Pos != 0 || data.maxPos != 1 {
			t.Fatalf("incorrect coil range: %d..%d", data.Pos, data.maxPos)
		}
		cfg.Registers[0].Data[0].last = time.Now()
		if !original.Registers[0].Data[0].last.IsZero() {
			t.Fatal("client changed source config timestamps")
		}
	}
}

func TestInvalidMQTTTopic(t *testing.T) {
	oldConfig, oldPath := config, *configFile
	t.Cleanup(func() { config, *configFile = oldConfig, oldPath })
	*configFile = t.TempDir() + "/config.json"
	if err := os.WriteFile(*configFile, []byte(`{"mqtt":{"lwt_topic":"status","state_topic":"state/#","set_topic":"set/"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := readConfig(); err == nil {
		t.Fatal("wildcard publication topic accepted")
	}
}
