package main

import "testing"

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
