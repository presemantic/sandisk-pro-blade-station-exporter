package device

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func add(t *testing.T, root, name, serial, identity string) {
	t.Helper()
	p := filepath.Join(root, "class/hidraw", name, "device")
	if e := os.MkdirAll(p, 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(p, "uevent"), []byte(fmt.Sprintf("HID_ID=%s\nHID_UNIQ=%s\nHID_PHYS=usb-%s/input0\n", identity, serial, name)), 0644); e != nil {
		t.Fatal(e)
	}
}
func TestDiscoverFleetAndIgnoreOtherDevices(t *testing.T) {
	root := t.TempDir()
	for i := range 100 {
		add(t, root, fmt.Sprintf("hidraw%d", i), fmt.Sprintf("private-serial-%d", i), HIDIdentity)
	}
	add(t, root, "hidraw101", "keyboard", "0003:00001234:00005678")
	devices, e := Discover(root, "/devices")
	if e != nil || len(devices) != 100 {
		t.Fatalf("count=%d err=%v", len(devices), e)
	}
	for _, d := range devices {
		if strings.Contains(d.ID, "private") || !strings.HasPrefix(d.Path, "/devices/hidraw") {
			t.Fatal(d)
		}
	}
}
func TestIdentitySurvivesRenumbering(t *testing.T) {
	r := t.TempDir()
	add(t, r, "hidraw1", "same", HIDIdentity)
	a, _ := Discover(r, "/dev")
	os.Rename(filepath.Join(r, "class/hidraw/hidraw1"), filepath.Join(r, "class/hidraw/hidraw9"))
	b, _ := Discover(r, "/dev")
	if a[0].ID != b[0].ID || a[0].Path == b[0].Path {
		t.Fatal("unstable device identity")
	}
}
func TestMissingAndDuplicate(t *testing.T) {
	d, e := Discover(t.TempDir(), "/dev")
	if e != nil || len(d) != 0 {
		t.Fatal(d, e)
	}
	r := t.TempDir()
	add(t, r, "hidraw1", "same", HIDIdentity)
	add(t, r, "hidraw2", "same", HIDIdentity)
	if _, e := Discover(r, "/dev"); e == nil {
		t.Fatal("accepted duplicate station identity")
	}
}
