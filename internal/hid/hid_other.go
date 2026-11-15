//go:build !linux

package hid

import (
	"context"
	"fmt"
	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/protocol"
	"time"
)

type Client struct{}

func Open(string, time.Duration) (*Client, error) {
	return nil, fmt.Errorf("hardware transport requires Linux")
}
func (*Client) Close() error { return nil }
func (*Client) SetFan(context.Context, bool, byte) error {
	return fmt.Errorf("hardware transport requires Linux")
}
func (*Client) Query(context.Context, protocol.Command, byte) ([]byte, error) {
	return nil, fmt.Errorf("hardware transport requires Linux")
}
