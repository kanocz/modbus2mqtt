package main

import (
	"testing"
)

func TestSetBit(t *testing.T) {

	tests := []struct {
		name string
		orig uint16
		pos  int
		want uint16
	}{
		{orig: 0, pos: 4, want: 16},
		{orig: 0b00001000, pos: 0, want: 0b00001001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SetBit(tt.orig, tt.pos); got != tt.want {
				t.Errorf("SetBit() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClearBit(t *testing.T) {
	tests := []struct {
		name string
		orig uint16
		pos  int
		want uint16
	}{
		{orig: 0, pos: 4, want: 0},
		{orig: 0b00001000, pos: 0, want: 0b00001000},
		{orig: 0b00001111, pos: 1, want: 0b00001101},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClearBit(tt.orig, tt.pos); got != tt.want {
				t.Errorf("ClearBit() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetXBit(t *testing.T) {
	type args struct {
		orig uint16
		val  uint16
		pos  int
		bits int
	}
	tests := []struct {
		name string
		args args
		want uint16
	}{
		{
			name: "zero",
			args: args{
				val:  0b0000_0000_0000_0000,
				orig: 0b0000_0000_0000_0000,
				pos:  0,
				bits: 0,
			},
			want: 0b0000_0000_0000_0000,
		},
		{
			name: "4bit4onzero",
			args: args{
				val:  0b0000_0000_0000_1111,
				orig: 0b0000_0000_0000_0000,
				pos:  4,
				bits: 4,
			},
			want: 0b0000_0000_1111_0000,
		},
		{
			name: "4_0_bit4on_ptn",
			args: args{
				val:  0b0000_0000_0000_0000,
				orig: 0b0101_0101_0101_0101,
				pos:  4,
				bits: 4,
			},
			want: 0b0101_0101_0000_0101,
		},
		{
			name: "4_1_bit4on_ptn",
			args: args{
				val:  0b1111_1111_1111_1111,
				orig: 0b0101_0101_1111_0101,
				pos:  4,
				bits: 4,
			},
			want: 0b0101_0101_1111_0101,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SetXBit(tt.args.orig, tt.args.val, tt.args.pos, tt.args.bits); got != tt.want {
				t.Errorf("SetXBit() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetXBit(t *testing.T) {
	type args struct {
		orig uint16
		pos  int
		bits int
	}
	tests := []struct {
		name string
		args args
		want uint16
	}{
		{
			name: "4bit_get",
			args: args{
				orig: 0b0101_0101_1001_0101,
				pos:  4,
				bits: 4,
			},
			want: 0b1001,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetXBit(tt.args.orig, tt.args.pos, tt.args.bits); got != tt.want {
				t.Errorf("GetXBit() = %v, want %v", got, tt.want)
			}
		})
	}
}
