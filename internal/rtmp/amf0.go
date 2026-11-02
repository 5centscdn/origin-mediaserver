package rtmp

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sort"
)

// AMF0 type markers
const (
	amf0Number      = 0x00
	amf0Boolean     = 0x01
	amf0String      = 0x02
	amf0Object      = 0x03
	amf0Null        = 0x05
	amf0Undefined   = 0x06
	amf0ECMAArray   = 0x08
	amf0ObjectEnd   = 0x09
	amf0StrictArray = 0x0A
	amf0Date        = 0x0B
	amf0LongString  = 0x0C
)

// AMF0Value is a discriminated union of all AMF0 types we need to handle.
type AMF0Value interface {
	amf0Marker() byte
}

type (
	AMF0Number  float64
	AMF0Boolean bool
	AMF0String  string
	AMF0Null    struct{}
	AMF0Object  map[string]AMF0Value
	AMF0Array   []AMF0Value
)

func (AMF0Number) amf0Marker() byte  { return amf0Number }
func (AMF0Boolean) amf0Marker() byte { return amf0Boolean }
func (AMF0String) amf0Marker() byte  { return amf0String }
func (AMF0Null) amf0Marker() byte    { return amf0Null }
func (AMF0Object) amf0Marker() byte  { return amf0Object }
func (AMF0Array) amf0Marker() byte   { return amf0StrictArray }

// ─── Decoder ────────────────────────────────────────────────────────────────

// DecodeAMF0 reads one AMF0 value from r.
func DecodeAMF0(r io.Reader) (AMF0Value, error) {
	marker := make([]byte, 1)
	if _, err := io.ReadFull(r, marker); err != nil {
		return nil, fmt.Errorf("read marker: %w", err)
	}

	switch marker[0] {
	case amf0Number:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		bits := binary.BigEndian.Uint64(b[:])
		return AMF0Number(math.Float64frombits(bits)), nil

	case amf0Boolean:
		b := make([]byte, 1)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		return AMF0Boolean(b[0] != 0), nil

	case amf0String:
		s, err := readAMF0ShortString(r)
		return AMF0String(s), err

	case amf0Null, amf0Undefined:
		return AMF0Null{}, nil

	case amf0Object:
		return readAMF0Object(r)

	case amf0ECMAArray:
		// 4-byte array count (hint only — we read until object-end marker)
		var count [4]byte
		if _, err := io.ReadFull(r, count[:]); err != nil {
			return nil, err
		}
		return readAMF0Object(r) // same wire format as Object after count

	case amf0StrictArray:
		var countBytes [4]byte
		if _, err := io.ReadFull(r, countBytes[:]); err != nil {
			return nil, err
		}
		count := binary.BigEndian.Uint32(countBytes[:])
		arr := make(AMF0Array, 0, count)
		for i := uint32(0); i < count; i++ {
			v, err := DecodeAMF0(r)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil

	case amf0LongString:
		s, err := readAMF0LongString(r)
		return AMF0String(s), err

	case amf0Date:
		var b [10]byte // 8-byte double + 2-byte timezone (ignored)
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		return AMF0Null{}, nil // dates not needed for RTMP control

	default:
		return nil, fmt.Errorf("unsupported AMF0 type 0x%02X", marker[0])
	}
}

// DecodeAMF0All reads all AMF0 values from data until EOF.
func DecodeAMF0All(data []byte) ([]AMF0Value, error) {
	r := newByteReader(data)
	var out []AMF0Value
	for r.remaining() > 0 {
		v, err := DecodeAMF0(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func readAMF0ShortString(r io.Reader) (string, error) {
	var lenBytes [2]byte
	if _, err := io.ReadFull(r, lenBytes[:]); err != nil {
		return "", err
	}
	length := binary.BigEndian.Uint16(lenBytes[:])
	if length == 0 {
		return "", nil
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readAMF0LongString(r io.Reader) (string, error) {
	var lenBytes [4]byte
	if _, err := io.ReadFull(r, lenBytes[:]); err != nil {
		return "", err
	}
	length := binary.BigEndian.Uint32(lenBytes[:])
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readAMF0Object(r io.Reader) (AMF0Object, error) {
	obj := make(AMF0Object)
	for {
		// Read key (short string without type marker)
		key, err := readAMF0ShortString(r)
		if err != nil {
			return nil, err
		}

		// Peek at next byte — if it's the object-end marker (0x09),
		// consume it and stop.
		marker := make([]byte, 1)
		if _, err := io.ReadFull(r, marker); err != nil {
			return nil, err
		}
		if marker[0] == amf0ObjectEnd {
			break
		}

		// Not the end marker — put it back by wrapping r in a peekable reader.
		// Since we can't unread, we pass the marker byte as a prefix.
		val, err := DecodeAMF0(&prefixReader{prefix: marker, r: r})
		if err != nil {
			return nil, fmt.Errorf("object value for %q: %w", key, err)
		}
		obj[key] = val
	}
	return obj, nil
}

// ─── Encoder ────────────────────────────────────────────────────────────────

// EncodeAMF0 serialises values into a byte slice.
func EncodeAMF0(vals ...AMF0Value) ([]byte, error) {
	var buf []byte
	for _, v := range vals {
		b, err := encodeOne(v)
		if err != nil {
			return nil, err
		}
		buf = append(buf, b...)
	}
	return buf, nil
}

func encodeOne(v AMF0Value) ([]byte, error) {
	switch val := v.(type) {
	case AMF0Number:
		b := make([]byte, 9)
		b[0] = amf0Number
		binary.BigEndian.PutUint64(b[1:], math.Float64bits(float64(val)))
		return b, nil

	case AMF0Boolean:
		if val {
			return []byte{amf0Boolean, 1}, nil
		}
		return []byte{amf0Boolean, 0}, nil

	case AMF0String:
		s := string(val)
		if len(s) > 65535 {
			// Long string
			b := make([]byte, 5+len(s))
			b[0] = amf0LongString
			binary.BigEndian.PutUint32(b[1:], uint32(len(s)))
			copy(b[5:], s)
			return b, nil
		}
		b := make([]byte, 3+len(s))
		b[0] = amf0String
		binary.BigEndian.PutUint16(b[1:], uint16(len(s)))
		copy(b[3:], s)
		return b, nil

	case AMF0Null:
		return []byte{amf0Null}, nil

	case AMF0Object:
		var buf []byte
		buf = append(buf, amf0Object)
		for _, k := range amf0KeyOrder(val) {
			fv := val[k]
			// Key without type marker
			buf = append(buf, byte(len(k)>>8), byte(len(k)))
			buf = append(buf, k...)
			fb, err := encodeOne(fv)
			if err != nil {
				return nil, err
			}
			buf = append(buf, fb...)
		}
		// Object-end marker
		buf = append(buf, 0x00, 0x00, amf0ObjectEnd)
		return buf, nil

	default:
		return nil, fmt.Errorf("unsupported AMF0 type %T", v)
	}
}

// amf0FieldRank is the key order FMS uses on the wire. Some clients parse
// status objects positionally rather than by key: FMLE 3.x exits outright
// when an _error object starts with "description" instead of "level", "code".
// Go map iteration is randomised, so without a fixed order that crash hit
// about half of all Adobe-auth attempts (captured in pcap, 2026-11-02).
var amf0FieldRank = map[string]int{
	// connect command object (client side, puller.go)
	"app": 1, "type": 2, "flashVer": 3, "swfUrl": 4, "tcUrl": 5,
	// connect _result properties
	"fmsVer": 10, "capabilities": 11, "mode": 12,
	// status / _result / _error info objects
	"level": 20, "code": 21, "description": 22, "objectEncoding": 23,
	"details": 24, "clientid": 25,
}

// amf0KeyOrder returns obj's keys with known keys first in FMS order and
// any others after them in alphabetical order, so encoding is deterministic.
func amf0KeyOrder(obj AMF0Object) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, iok := amf0FieldRank[keys[i]]
		rj, jok := amf0FieldRank[keys[j]]
		switch {
		case iok && jok:
			return ri < rj
		case iok != jok:
			return iok
		}
		return keys[i] < keys[j]
	})
	return keys
}

// ─── Helpers ────────────────────────────────────────────────────────────────

type byteReader struct {
	data []byte
	pos  int
}

func newByteReader(data []byte) *byteReader { return &byteReader{data: data} }
func (r *byteReader) remaining() int         { return len(r.data) - r.pos }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// prefixReader prepends one byte then delegates to r.
type prefixReader struct {
	prefix []byte
	r      io.Reader
	used   bool
}

func (p *prefixReader) Read(buf []byte) (int, error) {
	if !p.used && len(p.prefix) > 0 {
		p.used = true
		n := copy(buf, p.prefix)
		return n, nil
	}
	return p.r.Read(buf)
}
