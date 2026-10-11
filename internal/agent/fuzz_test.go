package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// FuzzUnauthenticatedPeer sends arbitrary bytes as the first thing on a
// connection. The token is random per run, so the fuzzer cannot produce it:
// every input is a peer without the token, and must get silence back.
func FuzzUnauthenticatedPeer(f *testing.F) {
	frame := func(t byte, p []byte) []byte {
		b := make([]byte, 5+len(p))
		b[0] = t
		binary.BigEndian.PutUint32(b[1:], uint32(len(p)))
		return append(b[:5], p...)
	}
	hello, _ := json.Marshal(Request{Token: "guess", Argv: []string{"true"}})
	f.Add(frame(fHello, hello))
	f.Add(frame(fHello, []byte("{")))
	f.Add(frame(fStdin, []byte("x")))
	f.Add([]byte{fHello, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{})

	var key [32]byte
	rand.Read(key[:])
	token := newTokenBox(hex.EncodeToString(key[:]))

	f.Fuzz(func(t *testing.T, b []byte) {
		c := &memConn{in: bytes.NewReader(b)}
		handle(newConn(c), token, "k", func() {})
		if c.out.Len() != 0 {
			t.Fatalf("answered a peer without the token: %q", c.out.Bytes())
		}
	})
}

// memConn is a connection whose peer has sent b and hung up, so a short
// input ends in EOF at once rather than waiting out the hello timeout.
type memConn struct {
	net.Conn // anything handle reaches beyond these panics, which the fuzzer reports
	in       *bytes.Reader
	out      bytes.Buffer
}

func (c *memConn) Read(p []byte) (int, error)       { return c.in.Read(p) }
func (c *memConn) Write(p []byte) (int, error)      { return c.out.Write(p) }
func (c *memConn) Close() error                     { return nil }
func (c *memConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memConn) SetWriteDeadline(time.Time) error { return nil }
func (c *memConn) SetDeadline(time.Time) error      { return nil }
