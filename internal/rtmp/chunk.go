package rtmp

import (
	"encoding/binary"
	"fmt"
	"io"
)

// RTMP chunk message type IDs
const (
	msgSetChunkSize     = 1
	msgAbort            = 2
	msgAck              = 3
	msgUserControl      = 4
	msgWindowAckSize    = 5
	msgSetPeerBandwidth = 6
	msgAudio            = 8
	msgVideo            = 9
	msgDataAMF3         = 15
	msgSharedObjAMF3    = 16
	msgCommandAMF3      = 17
	msgDataAMF0         = 18
	msgSharedObjAMF0    = 19
	msgCommandAMF0      = 20
	msgAggregate        = 22
)

// User Control event types (RTMP spec §7.1.7) that need a reply.
const (
	userCtrlPingRequest  = 6
	userCtrlPingResponse = 7
)

// pingResponse returns the User Control PingResponse payload answering
// data, or nil if data isn't a PingRequest. A peer sends PingRequest as a
// keepalive and drops the connection if no PingResponse with the same
// timestamp comes back — Zixi resets after ~20 s — so both the inbound
// connection and the outbound puller must answer it.
func pingResponse(data []byte) []byte {
	if len(data) < 6 || binary.BigEndian.Uint16(data) != userCtrlPingRequest {
		return nil
	}
	resp := make([]byte, 6)
	binary.BigEndian.PutUint16(resp, userCtrlPingResponse)
	copy(resp[2:], data[2:6])
	return resp
}

// chunkStreamState tracks partial-chunk reassembly state for one chunk stream ID.
// RTMP multiplexes multiple message streams over "chunk streams" (csid).
// A single logical message can be split across many chunks; we accumulate
// chunks here until the full message is ready.
type chunkStreamState struct {
	csid uint32

	// Header fields inherited from the most-recent non-Type3 chunk on this csid.
	timestamp       uint32
	timestampDelta  uint32
	messageLength   uint32
	messageTypeID   uint8
	messageStreamID uint32
	hasExtendedTS   bool

	// Partial message buffer.
	buf []byte
}

// Message is a fully reassembled RTMP message.
type Message struct {
	TypeID          uint8
	MessageStreamID uint32
	Timestamp       uint32
	Data            []byte
}

// ChunkDemuxer reassembles RTMP chunks into complete messages.
// RTMP uses a variable-format chunk header (fmt 0–3) that compresses
// repeated header fields when they don't change between chunks.
type ChunkDemuxer struct {
	r         io.Reader
	chunkSize uint32                   // negotiated chunk size (default 128)
	streams   map[uint32]*chunkStreamState // csid → state
}

func newChunkDemuxer(r io.Reader) *ChunkDemuxer {
	return &ChunkDemuxer{
		r:         r,
		chunkSize: 128,
		streams:   make(map[uint32]*chunkStreamState),
	}
}

// SetChunkSize updates the chunk size after receiving a Set Chunk Size message.
func (d *ChunkDemuxer) SetChunkSize(size uint32) {
	if size > 0 && size <= 0x7FFFFFFF {
		d.chunkSize = size
	}
}

// ReadMessage reads the next complete RTMP message from the stream.
// It transparently handles multi-chunk messages and all 4 chunk header formats.
func (d *ChunkDemuxer) ReadMessage() (*Message, error) {
	for {
		// ── Read Basic Header ────────────────────────────────────────────────
		b0 := make([]byte, 1)
		if _, err := io.ReadFull(d.r, b0); err != nil {
			return nil, fmt.Errorf("read basic header byte0: %w", err)
		}

		format := (b0[0] >> 6) & 0x03
		csidRaw := uint32(b0[0] & 0x3F)

		var csid uint32
		switch csidRaw {
		case 0: // 2-byte form: csid = second byte + 64
			b := make([]byte, 1)
			if _, err := io.ReadFull(d.r, b); err != nil {
				return nil, err
			}
			csid = uint32(b[0]) + 64
		case 1: // 3-byte form: csid = (third byte * 256) + second byte + 64
			b := make([]byte, 2)
			if _, err := io.ReadFull(d.r, b); err != nil {
				return nil, err
			}
			csid = uint32(b[1])*256 + uint32(b[0]) + 64
		default:
			csid = csidRaw
		}

		cs, ok := d.streams[csid]
		if !ok {
			cs = &chunkStreamState{csid: csid}
			d.streams[csid] = cs
		}

		// ── Read Message Header ──────────────────────────────────────────────
		switch format {
		case 0: // Type 0: 11 bytes — full header
			hdr := make([]byte, 11)
			if _, err := io.ReadFull(d.r, hdr); err != nil {
				return nil, fmt.Errorf("type0 header: %w", err)
			}
			ts := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])
			cs.messageLength = uint32(hdr[3])<<16 | uint32(hdr[4])<<8 | uint32(hdr[5])
			cs.messageTypeID = hdr[6]
			cs.messageStreamID = binary.LittleEndian.Uint32(hdr[7:11])
			cs.hasExtendedTS = ts == 0xFFFFFF
			if cs.hasExtendedTS {
				ext := make([]byte, 4)
				if _, err := io.ReadFull(d.r, ext); err != nil {
					return nil, err
				}
				cs.timestamp = binary.BigEndian.Uint32(ext)
			} else {
				cs.timestamp = ts
			}
			cs.buf = nil // new message starts fresh

		case 1: // Type 1: 7 bytes — no message stream ID
			hdr := make([]byte, 7)
			if _, err := io.ReadFull(d.r, hdr); err != nil {
				return nil, fmt.Errorf("type1 header: %w", err)
			}
			delta := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])
			cs.messageLength = uint32(hdr[3])<<16 | uint32(hdr[4])<<8 | uint32(hdr[5])
			cs.messageTypeID = hdr[6]
			cs.hasExtendedTS = delta == 0xFFFFFF
			if cs.hasExtendedTS {
				ext := make([]byte, 4)
				if _, err := io.ReadFull(d.r, ext); err != nil {
					return nil, err
				}
				cs.timestampDelta = binary.BigEndian.Uint32(ext)
			} else {
				cs.timestampDelta = delta
			}
			cs.timestamp += cs.timestampDelta
			cs.buf = nil

		case 2: // Type 2: 3 bytes — timestamp delta only
			hdr := make([]byte, 3)
			if _, err := io.ReadFull(d.r, hdr); err != nil {
				return nil, fmt.Errorf("type2 header: %w", err)
			}
			delta := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])
			cs.hasExtendedTS = delta == 0xFFFFFF
			if cs.hasExtendedTS {
				ext := make([]byte, 4)
				if _, err := io.ReadFull(d.r, ext); err != nil {
					return nil, err
				}
				cs.timestampDelta = binary.BigEndian.Uint32(ext)
			} else {
				cs.timestampDelta = delta
			}
			cs.timestamp += cs.timestampDelta

		case 3: // Type 3: no header — everything inherited
			// Extended timestamp re-read if the previous chunk used one.
			if cs.hasExtendedTS {
				ext := make([]byte, 4)
				if _, err := io.ReadFull(d.r, ext); err != nil {
					return nil, err
				}
				// Re-read but value should match; just consume it.
			}
		}

		// ── Read Chunk Data ──────────────────────────────────────────────────
		remaining := cs.messageLength - uint32(len(cs.buf))
		toRead := remaining
		if toRead > d.chunkSize {
			toRead = d.chunkSize
		}

		chunk := make([]byte, toRead)
		if _, err := io.ReadFull(d.r, chunk); err != nil {
			return nil, fmt.Errorf("chunk data: %w", err)
		}
		cs.buf = append(cs.buf, chunk...)

		// ── Message complete? ────────────────────────────────────────────────
		if uint32(len(cs.buf)) >= cs.messageLength {
			msg := &Message{
				TypeID:          cs.messageTypeID,
				MessageStreamID: cs.messageStreamID,
				Timestamp:       cs.timestamp,
				Data:            cs.buf,
			}
			cs.buf = nil
			return msg, nil
		}
		// Not complete yet — loop to read the next chunk.
	}
}
