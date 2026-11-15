// Package protocol implements the allowlisted read-only PRO-BLADE STATION protocol.
package protocol

import (
	"encoding/binary"
	"fmt"
)

const PacketSize = 64
const SupportedFirmware = "2.0.1.2"

type Command byte

const (
	GetFirmware     Command = 0
	GetStatus       Command = 1
	GetThermalTable Command = 10
)

// Request deliberately cannot construct reset, fan-control or firmware-write commands.
func Request(command Command, profile byte) ([PacketSize]byte, error) {
	var p [PacketSize]byte
	p[0], p[1], p[3], p[4] = 8, 2, 0, byte(command)
	switch command {
	case GetFirmware, GetStatus:
		if profile != 0 {
			return p, fmt.Errorf("profile is only valid for thermal tables")
		}
	case GetThermalTable:
		if profile > 6 {
			return p, fmt.Errorf("thermal profile %d outside 0..6", profile)
		}
		p[1], p[5] = 3, profile
	default:
		return p, fmt.Errorf("command %d is not an allowlisted GET", command)
	}
	for _, v := range p[3 : 3+int(p[1])] {
		p[2] += v
	}
	return p, nil
}

// Validate checks framing, response type, command, exact payload length and checksum.
func Validate(p []byte, command Command) error {
	if len(p) != PacketSize {
		return fmt.Errorf("response length %d, want %d", len(p), PacketSize)
	}
	if p[0] != 8 || p[3] != 1 || p[4] != byte(command) {
		return fmt.Errorf("unexpected response header")
	}
	expected := map[Command]byte{GetFirmware: 10, GetStatus: 17, GetThermalTable: 23}
	if n, ok := expected[command]; !ok || p[1] != n {
		return fmt.Errorf("unexpected payload length %d for command %d", p[1], command)
	}
	var sum byte
	for _, v := range p[3 : 3+int(p[1])] {
		sum += v
	}
	if sum != p[2] {
		return fmt.Errorf("response checksum mismatch")
	}
	return nil
}

type Firmware struct {
	Version string
	Raw     [8]byte
}

func DecodeFirmware(p []byte) (Firmware, error) {
	if err := Validate(p, GetFirmware); err != nil {
		return Firmware{}, err
	}
	f := Firmware{Version: fmt.Sprintf("%d.%d.%d.%d", p[5], p[6], p[7], p[8])}
	copy(f.Raw[:], p[5:13])
	return f, nil
}

type Status struct {
	FanRPM       uint16
	Temperatures [5]int16
	// Only fields linked to a known firmware function receive semantic names.
	// ControlInput is a GPIO-derived input; EnclosureState is a raw firmware enum.
	ControlInput   byte
	EnclosureState byte
	SlotState      byte
	Raw            [15]byte
}

func DecodeStatus(p []byte) (Status, error) {
	if err := Validate(p, GetStatus); err != nil {
		return Status{}, err
	}
	s := Status{FanRPM: binary.LittleEndian.Uint16(p[5:7]), ControlInput: p[17], EnclosureState: p[18], SlotState: p[19]}
	for i := range s.Temperatures {
		s.Temperatures[i] = int16(binary.LittleEndian.Uint16(p[7+i*2 : 9+i*2]))
	}
	copy(s.Raw[:], p[5:20])
	return s, nil
}

type ThermalTable [4][5]byte

func DecodeThermalTable(p []byte) (ThermalTable, error) {
	var t ThermalTable
	if err := Validate(p, GetThermalTable); err != nil {
		return t, err
	}
	// Firmware leaves response byte 5 zero: it is NOT an echoed profile ID.
	for row := range t {
		copy(t[row][:], p[6+row*5:11+row*5])
	}
	return t, nil
}
