package protocol

import (
	"encoding/hex"
	"testing"
)

func packet(t *testing.T, s string) []byte {
	t.Helper()
	b, e := hex.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	p := make([]byte, 64)
	copy(p, b)
	return p
}
func TestObservedPackets(t *testing.T) {
	fw := packet(t, "080a0c01000200010200000006")
	f, e := DecodeFirmware(fw)
	if e != nil || f.Version != "2.0.1.2" {
		t.Fatalf("firmware %+v %v", f, e)
	}
	s, e := DecodeStatus(packet(t, "081123010100003800310030002b002900010033"))
	if e != nil || s.FanRPM != 0 || s.Temperatures != [5]int16{56, 49, 48, 43, 41} || s.SlotState != 51 {
		t.Fatalf("status %+v %v", s, e)
	}
	table, e := DecodeThermalTable(packet(t, "0817f5010a0041232323234b29292929502b2b2b2b5a36363636"))
	if e != nil || table[0] != [5]byte{65, 35, 35, 35, 35} || table[3][4] != 54 {
		t.Fatalf("table %+v %v", table, e)
	}
}
func TestReadOnlyRequestAllowlist(t *testing.T) {
	for cmd := range 256 {
		_, err := Request(Command(cmd), 0)
		allowed := cmd == 0 || cmd == 1 || cmd == 10
		if (err == nil) != allowed {
			t.Fatalf("unexpected command access %d", cmd)
		}
	}
	for profile := range byte(7) {
		p, err := Request(GetThermalTable, profile)
		if err != nil || p[2] != 10+profile || p[3] != 0 || p[5] != profile {
			t.Fatalf("bad request %x %v", p, err)
		}
	}
	if _, err := Request(GetThermalTable, 7); err == nil {
		t.Fatal("out-of-range profile accepted")
	}
}
func TestRejectMalformed(t *testing.T) {
	valid := packet(t, "081123010100003800310030002b002900010033")
	for _, index := range []int{0, 1, 2, 3, 4, 8} {
		p := append([]byte(nil), valid...)
		p[index] ^= 1
		if _, err := DecodeStatus(p); err == nil {
			t.Fatalf("accepted corruption at %d", index)
		}
	}
	for _, length := range []int{0, 4, 20, 63} {
		if _, err := DecodeStatus(valid[:length]); err == nil {
			t.Fatal("short packet accepted")
		}
	}
}
func FuzzDecode(f *testing.F) {
	f.Add(make([]byte, 64))
	f.Add([]byte{8, 17, 0, 1, 1})
	f.Fuzz(func(t *testing.T, b []byte) { DecodeStatus(b); DecodeFirmware(b); DecodeThermalTable(b) })
}
