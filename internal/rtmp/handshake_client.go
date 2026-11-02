package rtmp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// clientHandshake performs the RTMP handshake as the client (the reverse
// read/write order of doHandshake, which only handles the server role).
//
// Sequence:
//
//	Client → Server : C0 (1 byte version) + C1 (1536 bytes)
//	Server → Client : S0 (1 byte version) + S1 (1536 bytes) + S2 (echo of C1)
//	Client → Server : C2 (echo of S1)
func clientHandshake(rw io.ReadWriter) error {
	// ── Send C0 + C1 ──────────────────────────────────────────────────────────
	c0c1 := make([]byte, 1+handshakeLen)
	c0c1[0] = rtmpVersion
	binary.BigEndian.PutUint32(c0c1[1:5], uint32(time.Now().Unix()))
	// bytes [5:9] are zero per spec
	if _, err := rand.Read(c0c1[9:]); err != nil {
		return fmt.Errorf("rand C1: %w", err)
	}
	if _, err := rw.Write(c0c1); err != nil {
		return fmt.Errorf("write C0C1: %w", err)
	}

	// ── Receive S0 ────────────────────────────────────────────────────────────
	s0 := make([]byte, 1)
	if _, err := io.ReadFull(rw, s0); err != nil {
		return fmt.Errorf("read S0: %w", err)
	}
	if s0[0] != rtmpVersion {
		return fmt.Errorf("unsupported RTMP version %d from server", s0[0])
	}

	// ── Receive S1 ────────────────────────────────────────────────────────────
	s1 := make([]byte, handshakeLen)
	if _, err := io.ReadFull(rw, s1); err != nil {
		return fmt.Errorf("read S1: %w", err)
	}

	// ── Receive S2 ────────────────────────────────────────────────────────────
	// We don't validate S2 against our C1 — many servers don't echo it
	// faithfully either, mirroring doHandshake's own leniency on C2.
	s2 := make([]byte, handshakeLen)
	if _, err := io.ReadFull(rw, s2); err != nil {
		return fmt.Errorf("read S2: %w", err)
	}

	// ── Send C2 = echo of S1 ──────────────────────────────────────────────────
	if _, err := rw.Write(s1); err != nil {
		return fmt.Errorf("write C2: %w", err)
	}

	return nil
}
