package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// SocketPath is where the listener for a launch dir binds (ADR-0003):
// $XDG_RUNTIME_DIR/atlas/<16 hex of sha256(launch dir)>.sock, else
// $TMPDIR/atlas-<uid>/….
func SocketPath(launchDir string) string {
	sum := sha256.Sum256([]byte(launchDir))
	name := hex.EncodeToString(sum[:])[:16] + ".sock"
	if d := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(d) {
		return filepath.Join(d, "atlas", name)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("atlas-%d", os.Getuid()), name)
}

// ListenUnix binds the socket in a 0700 directory with mode 0600 and returns a
// listener that only hands out connections from the same uid.
func ListenUnix(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a private directory needs its search bit
		return nil, err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is not a private directory", dir)
	}
	if c, err := net.Dial("unix", path); err == nil {
		_ = c.Close()
		return nil, fmt.Errorf("another atlas listener is serving %s", path)
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return &peerListener{Listener: l, uid: os.Getuid()}, nil
}

// peerListener drops connections whose peer is another user.
type peerListener struct {
	net.Listener
	uid int
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			_ = c.Close()
			continue
		}
		uid, err := peerUID(uc)
		if err != nil || uid != l.uid {
			_ = c.Close()
			continue
		}
		return c, nil
	}
}
