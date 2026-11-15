//go:build !linux

package control

import (
	"os"
	"sync"
)

var testLock sync.Mutex

func withLock(dir string, f func() error) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	testLock.Lock()
	defer testLock.Unlock()
	return f()
}
