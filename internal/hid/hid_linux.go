//go:build linux

// Package hid is the Linux transport. It exposes only the protocol's GET allowlist.
package hid

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"github.com/presemantic/sandisk-pro-blade-station-exporter/internal/protocol"
	"golang.org/x/sys/unix"
)

type Client struct {
	mu      sync.Mutex
	fd      int
	timeout time.Duration
}

func Open(path string, timeout time.Duration) (*Client, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open HID: %w", err)
	}
	fail := func(e error) (*Client, error) { unix.Close(fd); return nil, e }
	stat := unix.Stat_t{}
	if err := unix.Fstat(fd, &stat); err != nil {
		return fail(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		return fail(fmt.Errorf("HID path is not a character device"))
	}
	// Verify the opened FD too, preventing a hotplug race from selecting another HID.
	var info [8]byte
	const getRawInfo = uintptr(2<<30 | 8<<16 | 'H'<<8 | 3)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), getRawInfo, uintptr(unsafe.Pointer(&info[0])))
	if errno != 0 {
		return fail(fmt.Errorf("HID identity ioctl: %w", errno))
	}
	if binary.LittleEndian.Uint32(info[:4]) != 3 || binary.LittleEndian.Uint16(info[4:6]) != 0x4791 || binary.LittleEndian.Uint16(info[6:8]) != 0x2064 {
		return fail(fmt.Errorf("opened HID is not a PRO-BLADE Station"))
	}
	return &Client{fd: fd, timeout: timeout}, nil
}
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return nil
	}
	err := unix.Close(c.fd)
	c.fd = -1
	return err
}

// SetFan is the separately opted-in report-6 command verified on firmware 2.0.1.2.
// Manual=false restores the firmware controller; this never writes flash or tables.
func (c *Client) SetFan(ctx context.Context, manual bool, duty byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return os.ErrClosed
	}
	mode := byte(0)
	if manual {
		mode = 1
	} else {
		duty = 0
	}
	deadline := time.Now().Add(c.timeout)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	for {
		if err := wait(ctx, c.fd, unix.POLLOUT, deadline); err != nil {
			return err
		}
		n, err := unix.Write(c.fd, []byte{6, mode, duty})
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n != 3 {
			return fmt.Errorf("short fan-control write")
		}
		return nil
	}
}
func (c *Client) Query(ctx context.Context, command protocol.Command, profile byte) ([]byte, error) {
	packet, err := protocol.Request(command, profile)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return nil, os.ErrClosed
	}
	var buf [256]byte
	// Bounded draining ignores obsolete replies from a previous timed-out request.
	for range 16 {
		_, err = unix.Read(c.fd, buf[:])
		if errors.Is(err, unix.EAGAIN) {
			break
		}
		if err != nil && !errors.Is(err, unix.EINTR) {
			return nil, err
		}
	}
	deadline := time.Now().Add(c.timeout)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	for {
		if err = wait(ctx, c.fd, unix.POLLOUT, deadline); err != nil {
			return nil, err
		}
		n, e := unix.Write(c.fd, packet[:])
		if errors.Is(e, unix.EAGAIN) || errors.Is(e, unix.EINTR) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if n != len(packet) {
			return nil, fmt.Errorf("short HID write")
		}
		break
	}
	for {
		if err = wait(ctx, c.fd, unix.POLLIN, deadline); err != nil {
			return nil, err
		}
		n, e := unix.Read(c.fd, buf[:])
		if errors.Is(e, unix.EAGAIN) || errors.Is(e, unix.EINTR) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if n == 0 {
			return nil, fmt.Errorf("HID disconnected")
		}
		if n < 5 || buf[0] != 8 || buf[3] != 1 || buf[4] != byte(command) {
			continue
		}
		response := append([]byte(nil), buf[:n]...)
		if err = protocol.Validate(response, command); err != nil {
			return nil, err
		}
		return response, nil
	}
}
func wait(ctx context.Context, fd int, events int16, deadline time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return context.DeadlineExceeded
		}
		ms := int(min(left.Milliseconds()+1, 100))
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
		n, err := unix.Poll(fds, ms)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return fmt.Errorf("HID poll disconnected")
		}
		if fds[0].Revents&events != 0 {
			return nil
		}
	}
}
