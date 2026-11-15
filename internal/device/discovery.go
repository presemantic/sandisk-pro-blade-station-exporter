// Package device discovers stations by USB identity, independently of hidraw numbering.
package device

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const HIDIdentity = "0003:00004791:00002064"

type Device struct{ ID, Path, Location string }

func Discover(sysRoot, devRoot string) ([]Device, error) {
	entries, err := os.ReadDir(filepath.Join(sysRoot, "class/hidraw"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]Device, 0)
	seen := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "hidraw") || strings.ContainsAny(strings.TrimPrefix(name, "hidraw"), "/\\.") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(sysRoot, "class/hidraw", name, "device/uevent"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read HID identity: %w", err)
		}
		env := map[string]string{}
		for _, line := range strings.Split(string(raw), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if ok {
				env[k] = v
			}
		}
		if strings.ToUpper(env["HID_ID"]) != HIDIdentity {
			continue
		}
		location := env["HID_PHYS"]
		identity := env["HID_UNIQ"]
		if identity == "" {
			identity = location
		}
		if identity == "" {
			return nil, fmt.Errorf("station has neither serial nor physical identity")
		}
		hash := sha256.Sum256([]byte(identity))
		id := "station-" + hex.EncodeToString(hash[:12])
		if seen[id] {
			return nil, fmt.Errorf("duplicate station identity; provide unique hardware serials")
		}
		seen[id] = true
		result = append(result, Device{ID: id, Path: filepath.Join(devRoot, name), Location: location})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
