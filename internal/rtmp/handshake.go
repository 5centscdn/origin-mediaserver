package rtmp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// RTMP handshake constants
const (
	rtmpVersion  = 3
	handshakeLen = 1536
)

// doHandshake performs the RTMP C0+C1+C2 / S0+S1+S2 exchange.
//
// Sequence:
//   Client → Server : C0 (1 byte version) + C1 (1536 bytes)
//   Server → Client : S0 (1 byte version) + S1 (1536 bytes) + S2 (echo of C1)
//   Client → Server : C2 (echo of S1)
func doHandshake(rw io.ReadWriter) error {
	// ── Receive C0 ──────────────────────────────────────────────────────────
	c0 := make([]byte, 1)
	if _, err := io.ReadFull(rw, c0); err != nil {
		return fmt.Errorf("read C0: %w", err)
	}
	if c0[0] != rtmpVersion {
		return fmt.Errorf("unsupported RTMP version %d", c0[0])
	}

	// ── Receive C1 ──────────────────────────────────────────────────────────
	c1 := make([]byte, handshakeLen)
	if _, err := io.ReadFull(rw, c1); err != nil {
		return fmt.Errorf("read C1: %w", err)
	}

	// ── Send S0 + S1 + S2 ───────────────────────────────────────────────────
	s0 := []byte{rtmpVersion}

	s1 := make([]byte, handshakeLen)
	binary.BigEndian.PutUint32(s1[0:4], uint32(time.Now().Unix()))
	binary.BigEndian.PutUint32(s1[4:8], 0) // zeros per spec
	if _, err := rand.Read(s1[8:]); err != nil {
		return fmt.Errorf("rand S1: %w", err)
	}

	// S2 is a copy of C1 with our timestamp echo in bytes 4-7.
	s2 := make([]byte, handshakeLen)
	copy(s2, c1)
	binary.BigEndian.PutUint32(s2[4:8], uint32(time.Now().Unix()))

	out := make([]byte, 1+handshakeLen+handshakeLen)
	copy(out[0:], s0)
	copy(out[1:], s1)
	copy(out[1+handshakeLen:], s2)

	if _, err := rw.Write(out); err != nil {
		return fmt.Errorf("write S0S1S2: %w", err)
	}

	// ── Receive C2 ──────────────────────────────────────────────────────────
	c2 := make([]byte, handshakeLen)
	if _, err := io.ReadFull(rw, c2); err != nil {
		return fmt.Errorf("read C2: %w", err)
	}

	// We do not validate C2 contents — many encoders send a garbage C2.
	return nil
}
