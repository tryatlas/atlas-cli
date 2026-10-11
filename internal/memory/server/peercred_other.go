//go:build !darwin && !linux

package server

import (
	"errors"
	"net"
)

var errNoPeerCred = errors.New("peer credentials unavailable")

// peerUID is unsupported here (ADR-0006: no Windows in v1).
func peerUID(*net.UnixConn) (int, error) { return -1, errNoPeerCred }
