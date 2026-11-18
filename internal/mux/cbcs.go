package mux

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"log/slog"
	"sync"
)

// Common Encryption 'cbcs' (ISO/IEC 23001-7) for fMP4 tracks: the one
// scheme Widevine, PlayReady 4+ and FairPlay all accept, so HLS fMP4,
// LL-HLS, DASH and nDVR share one encrypted output. Keys come from
// internal/drm (castLabs DRMtoday CPIX).
//
// Layout choices (the same as Shaka Packager's cbcs output):
//   - Constant IV (tenc default_constant_IV, Per_Sample_IV_Size 0), reset
//     at the start of every subsample, so senc carries no IVs.
//   - Video (H.264, AVCC with 4-byte lengths): each VCL NAL is one
//     subsample whose clear part is the length prefix, NAL header and slice
//     header (TrackEncryption.Params, media.H264ParamSets in practice);
//     the rest is encrypted
//     with the 1:9 pattern (1 block encrypted, 9 skipped, CBC chained
//     across the encrypted blocks). Non-VCL NALs (SPS, PPS, SEI, AUD) and
//     slices with fewer than 16 bytes of data stay clear. A slice whose
//     header can't be parsed is still encrypted, after a fixed
//     fallbackClearLeader: a slice that then fails to decode is better
//     than DRM content sent in the clear.
//   - Audio: every 16-byte block of the sample encrypted (pattern 0:0),
//     signalled as one subsample per sample so audio and video share the
//     senc/saiz layout.
//   - A trailing partial block is always left clear (cbcs rule), so sample
//     sizes never change.
//
// Scope: H.264 video and AAC audio. HEVC (no hvc1 entry yet) and text
// tracks aren't encrypted; callers must not put other codecs in an
// encrypted presentation (internal/streamoutputs drops them).

// TrackEncryption makes an FMP4Track cbcs-encrypted. Set it on the track
// before writing the init segment; the init segment then carries
// encv/enca + sinf/tenc and the PSSH boxes, and every media segment is
// encrypted with senc/saiz/saio.
type TrackEncryption struct {
	KID  [16]byte
	Key  [16]byte
	IV   [16]byte // cbcs constant IV
	PSSH [][]byte // complete 'pssh' boxes for moov (from CPIX)

	// Params finds H.264 slice header lengths; required for video tracks.
	// It's media.NewH264ParamSets() (mux can't import media: media →
	// scte35 → mux). The track's SPS/PPS and any in-band ones are fed to
	// it.
	Params H264HeaderParser

	mu        sync.Mutex
	block     cipher.Block
	seeded    bool
	warnedHdr bool
}

// H264HeaderParser is implemented by media.H264ParamSets.
type H264HeaderParser interface {
	Add(nal []byte)
	SliceHeaderSize(nal []byte) (int, bool)
}

// fallbackClearLeader is the clear prefix of a slice whose header length
// is unknown: the 32 bytes Apple's SAMPLE-AES leaves clear, which covers
// the slice header of most real encoder output.
const fallbackClearLeader = 32

// cbcs pattern for video (crypt, skip) in 16-byte blocks.
const (
	cbcsVideoCrypt = 1
	cbcsVideoSkip  = 9
)

// subsample is one senc subsample entry.
type subsample struct {
	clear     uint32 // written as uint16; split by addClear
	protected uint32
}

func (e *TrackEncryption) init(t *FMP4Track) error {
	if e.block == nil {
		b, err := aes.NewCipher(e.Key[:])
		if err != nil {
			return err
		}
		e.block = b
	}
	if t.Type == TrackVideo && !e.seeded {
		if e.Params == nil {
			return errors.New("fmp4: encrypted video track needs TrackEncryption.Params")
		}
		e.Params.Add(t.SPS)
		e.Params.Add(t.PPS)
		e.seeded = true
	}
	return nil
}

// encryptSamples returns encrypted copies of samples (the originals are
// shared with other outputs through the Bus and must not change) and each
// sample's subsample map.
func (e *TrackEncryption) encryptSamples(t *FMP4Track, samples []Sample) ([]Sample, [][]subsample, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.init(t); err != nil {
		return nil, nil, err
	}
	out := make([]Sample, len(samples))
	subs := make([][]subsample, len(samples))
	for i, s := range samples {
		out[i] = s
		data := append([]byte(nil), s.Data...)
		if t.Type == TrackVideo {
			subs[i] = e.encryptVideo(data)
		} else {
			n := len(data) &^ 15
			cbcsEncrypt(e.block, e.IV, data[:n], 0, 0)
			subs[i] = []subsample{{clear: 0, protected: uint32(len(data))}}
		}
		out[i].Data = data
	}
	return out, subs, nil
}

// encryptVideo encrypts an AVCC (4-byte length) access unit in place.
func (e *TrackEncryption) encryptVideo(au []byte) []subsample {
	var subs []subsample
	pendingClear := 0
	off := 0
	for off+4 <= len(au) {
		n := int(binary.BigEndian.Uint32(au[off:]))
		if n <= 0 || off+4+n > len(au) {
			// Malformed length: leave the rest clear rather than guess.
			pendingClear += len(au) - off
			off = len(au)
			break
		}
		nal := au[off+4 : off+4+n]
		hdr := 0
		switch nal[0] & 0x1F {
		case 7, 8:
			e.Params.Add(nal)
		case 1, 5:
			if h, ok := e.Params.SliceHeaderSize(nal); ok {
				hdr = h
			} else {
				hdr = min(fallbackClearLeader, n)
				if !e.warnedHdr {
					e.warnedHdr = true
					slog.Warn("cbcs: couldn't parse an H.264 slice header (missing SPS/PPS?); encrypting after a fixed 32-byte clear leader")
				}
			}
		}
		if hdr > 0 && n-hdr >= 16 {
			prot := au[off+4+hdr : off+4+n]
			cbcsEncrypt(e.block, e.IV, prot[:len(prot)&^15], cbcsVideoCrypt, cbcsVideoSkip)
			subs = addSubsample(subs, pendingClear+4+hdr, n-hdr)
			pendingClear = 0
		} else {
			pendingClear += 4 + n
		}
		off += 4 + n
	}
	pendingClear += len(au) - off
	if pendingClear > 0 || len(subs) == 0 {
		subs = addSubsample(subs, pendingClear, 0)
	}
	return subs
}

// addSubsample appends {clear, protected}, splitting a clear run longer
// than a uint16 into clear-only entries first.
func addSubsample(subs []subsample, clear, protected int) []subsample {
	for clear > 0xFFFF {
		subs = append(subs, subsample{clear: 0xFFFF})
		clear -= 0xFFFF
	}
	return append(subs, subsample{clear: uint32(clear), protected: uint32(protected)})
}

// cbcsEncrypt encrypts data (a whole number of blocks) in place with
// AES-CBC from iv, applying the crypt:skip pattern; crypt=skip=0 means
// every block. CBC chains across the encrypted blocks only.
func cbcsEncrypt(b cipher.Block, iv [16]byte, data []byte, crypt, skip int) {
	if len(data) == 0 {
		return
	}
	enc := cipher.NewCBCEncrypter(b, iv[:])
	if crypt == 0 && skip == 0 {
		enc.CryptBlocks(data, data)
		return
	}
	stride := (crypt + skip) * 16
	for off := 0; off < len(data); off += stride {
		end := off + crypt*16
		if end > len(data) {
			end = len(data)
		}
		enc.CryptBlocks(data[off:end], data[off:end])
	}
}

// ─── boxes ───────────────────────────────────────────────────────────────

// writeSinf writes the ProtectionSchemeInfoBox for an encv/enca entry.
func writeSinf(w *bytes.Buffer, t *FMP4Track, origFormat string) {
	e := t.Encryption
	var sinf bytes.Buffer
	writeBox(&sinf, "frma", []byte(origFormat))
	writeBox(&sinf, "schm", []byte{0, 0, 0, 0, 'c', 'b', 'c', 's', 0, 1, 0, 0})

	var tenc bytes.Buffer
	tenc.Write([]byte{1, 0, 0, 0}) // version 1, flags
	tenc.WriteByte(0)              // reserved
	if t.Type == TrackVideo {
		tenc.WriteByte(cbcsVideoCrypt<<4 | cbcsVideoSkip)
	} else {
		tenc.WriteByte(0) // full-sample encryption
	}
	tenc.WriteByte(1) // default_isProtected
	tenc.WriteByte(0) // default_Per_Sample_IV_Size: constant IV
	tenc.Write(e.KID[:])
	tenc.WriteByte(16) // default_constant_IV_size
	tenc.Write(e.IV[:])
	var schi bytes.Buffer
	writeBox(&schi, "tenc", tenc.Bytes())
	writeBox(&sinf, "schi", schi.Bytes())
	writeBox(w, "sinf", sinf.Bytes())
}

// sencInfo is what writeTraf needs to write saiz/saio/senc.
type sencInfo struct {
	subs [][]subsample
}

// writeSencBoxes appends saiz, saio and senc to a traf body and returns the
// offset (within w) of saio's offset field and of senc's first sample
// entry, which WriteMediaSegment patches into saio once the moof's start
// is known.
func writeSencBoxes(w *bytes.Buffer, info *sencInfo) (saioField, sencData int) {
	// saiz: per-sample aux info size (subsample_count + 6 bytes per entry).
	var saiz bytes.Buffer
	saiz.Write([]byte{0, 0, 0, 0})
	saiz.WriteByte(0) // default_sample_info_size: sizes listed
	binary.Write(&saiz, binary.BigEndian, uint32(len(info.subs)))
	for _, s := range info.subs {
		saiz.WriteByte(byte(2 + 6*len(s)))
	}
	writeBox(w, "saiz", saiz.Bytes())

	// saio: one offset (all of senc's sample entries are contiguous).
	var saio bytes.Buffer
	saio.Write([]byte{0, 0, 0, 0})
	binary.Write(&saio, binary.BigEndian, uint32(1))
	saioLocal := saio.Len()
	binary.Write(&saio, binary.BigEndian, uint32(0))
	saioStart := w.Len()
	writeBox(w, "saio", saio.Bytes())

	var senc bytes.Buffer
	senc.Write([]byte{0, 0, 0, 0x02}) // flags: UseSubSampleEncryption
	binary.Write(&senc, binary.BigEndian, uint32(len(info.subs)))
	sencLocal := senc.Len()
	for _, s := range info.subs {
		binary.Write(&senc, binary.BigEndian, uint16(len(s)))
		for _, ss := range s {
			binary.Write(&senc, binary.BigEndian, uint16(ss.clear))
			binary.Write(&senc, binary.BigEndian, ss.protected)
		}
	}
	sencStart := w.Len()
	writeBox(w, "senc", senc.Bytes())
	return saioStart + 8 + saioLocal, sencStart + 8 + sencLocal
}

// writePSSH appends the encrypted tracks' pssh boxes to moov, once each.
func writePSSH(w *bytes.Buffer, tracks []*FMP4Track) {
	seen := map[string]bool{}
	for _, t := range tracks {
		if t.Encryption == nil {
			continue
		}
		for _, p := range t.Encryption.PSSH {
			if len(p) < 8 || seen[string(p)] {
				continue
			}
			seen[string(p)] = true
			w.Write(p)
		}
	}
}

// encryptable reports whether t's codec can carry cbcs here.
func encryptable(t *FMP4Track) bool {
	if t.Encryption == nil {
		return false
	}
	switch t.Type {
	case TrackVideo:
		// H.264 only (an HEVC SPS would land in avcC; there's no hvc1/encv
		// for HEVC yet).
		return len(t.SPS) > 0 && t.SPS[0]&0x1F == 7
	case TrackAudio:
		return t.AudioObjectType == 0 || t.AudioObjectType == AACAudioObjectType
	}
	return false
}
