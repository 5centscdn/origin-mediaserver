package mux

import (
	"bytes"
	"encoding/binary"
	"io"
)

// FMP4Muxer writes fragmented MP4 (ISO BMFF) for HLS fMP4 / CMAF / DASH.
// One init segment is written once; then one media segment per GOP or
// target duration interval.
//
// Box hierarchy reminder:
//   init: ftyp + moov (mvhd + trak(tkhd+mdia(mdhd+hdlr+minf(vmhd/smhd+dinf+stbl))) + mvex)
//   media segment: moof (mfhd + traf(tfhd+tfdt+trun)) + mdat

// TrackType selects video or audio track parameters.
type TrackType uint8

const (
	TrackVideo TrackType = iota
	TrackAudio
	TrackText // XML subtitle (EBU-TT-D) — see writeSTPP
)

// esds objectTypeIndication values (ISO/IEC 14496-1 Table 5) writeMP4A
// selects between — see FMP4Track.AudioObjectType. Exported: callers
// building an audio FMP4Track (internal/dash, internal/hls) need to set
// this explicitly for a non-AAC codec.
const (
	AACAudioObjectType   = 0x40 // Audio ISO/IEC 14496-3 (AAC)
	MPEG1AudioObjectType = 0x6B // Audio ISO/IEC 11172-3 (MPEG-1 Audio Layer I/II/III, i.e. MP2)
)

// FMP4Track describes one elementary stream in the fMP4 container.
type FMP4Track struct {
	Type       TrackType
	TrackID    uint32
	Timescale  uint32 // 90000 for video, sample rate for audio, 90000 for text (matches DTS/PTS ticks)
	Width      uint16 // video only
	Height     uint16 // video only
	SPS        []byte // H.264 SPS NAL unit (without start code)
	PPS        []byte // H.264 PPS NAL unit (without start code)
	AACConfig  []byte // AudioSpecificConfig for AAC — empty for MPEG-1/2 Audio (MP2), which has no equivalent
	SampleRate uint32 // audio only
	Channels   uint8  // audio only

	// AudioObjectType is the esds DecoderConfigDescriptor's
	// objectTypeIndication (ISO/IEC 14496-1 Table 5) for an audio track:
	// 0x40 (Audio ISO/IEC 14496-3, i.e. AAC — the default, used when this
	// field is left zero, for backward compatibility) or 0x6B (Audio
	// ISO/IEC 11172-3, MPEG-1 Audio Layer I/II/III — MP2 passthrough, see
	// internal/ingest/mpegts/mp2.go). Ignored for non-audio tracks.
	AudioObjectType byte

	// Namespace is the XML namespace list for a TrackText's stpp sample
	// entry (ISO/IEC 14496-30 XMLSubtitleSampleEntry) — space-separated,
	// e.g. "http://www.w3.org/ns/ttml". Only used when TextFormat is
	// "stpp" (the default when empty).
	Namespace string

	// TextFormat selects the TrackText sample entry: "stpp" (default when
	// empty — EBU-TT-D/XMLSubtitleSampleEntry, see writeSTPP) or "wvtt"
	// (WebVTT/WVTTSampleEntry, see writeWVTT and internal/webvtt). Ignored
	// for non-text tracks.
	TextFormat string
}

// Sample represents one coded frame (video access unit or audio frame).
type Sample struct {
	DTS      int64 // decoding timestamp, in track timescale units
	PTS      int64 // presentation timestamp (CTO = PTS - DTS)
	Duration uint32
	IsKey    bool
	Data     []byte
}

// FMP4Muxer serializes track data to ISO BMFF fragmented MP4.
type FMP4Muxer struct {
	tracks []*FMP4Track
	seqNum uint32
}

// NewFMP4Muxer creates a muxer with the given tracks.
func NewFMP4Muxer(tracks ...*FMP4Track) *FMP4Muxer {
	return &FMP4Muxer{tracks: tracks}
}

// WriteInitSegment writes the ftyp + moov init segment to w.
// This must be written exactly once before any media segments.
func (m *FMP4Muxer) WriteInitSegment(w io.Writer) error {
	var buf bytes.Buffer
	if err := writeFtyp(&buf); err != nil {
		return err
	}
	if err := m.writeMoov(&buf); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// WriteMediaSegment writes one moof+mdat pair to w.
// samples must be in decode-timestamp order.
func (m *FMP4Muxer) WriteMediaSegment(w io.Writer, trackIdx int, samples []Sample, baseDecodeTime int64) error {
	if trackIdx >= len(m.tracks) {
		return nil
	}
	track := m.tracks[trackIdx]

	m.seqNum++

	var buf bytes.Buffer
	dataOffsetPos, err := m.writeMoof(&buf, track, samples, m.seqNum, baseDecodeTime)
	if err != nil {
		return err
	}

	// trun's data_offset is measured from the start of this moof box (tfhd's
	// default-base-is-moof flag) to the first sample byte in the mdat that
	// follows — i.e. the size of the moof box we just wrote, plus mdat's own
	// 8-byte size+type header.
	moofSize := buf.Len()
	dataOffset := uint32(moofSize + 8)
	binary.BigEndian.PutUint32(buf.Bytes()[dataOffsetPos:dataOffsetPos+4], dataOffset)

	// mdat
	mdatSize := 8
	for _, s := range samples {
		mdatSize += len(s.Data)
	}
	writeUint32(&buf, uint32(mdatSize))
	buf.WriteString("mdat")
	for _, s := range samples {
		buf.Write(s.Data)
	}

	_, err = w.Write(buf.Bytes())
	return err
}

// ─── ftyp ────────────────────────────────────────────────────────────────────

func writeFtyp(w *bytes.Buffer) error {
	// ftyp: iso5 iso6 mp41 (CMAF-compatible)
	return writeBox(w, "ftyp", []byte{
		'i', 's', 'o', '5', // major brand
		0x00, 0x00, 0x00, 0x00, // minor version
		'i', 's', 'o', '5', // compatible brand
		'i', 's', 'o', '6',
		'm', 'p', '4', '1',
		'd', 'a', 's', 'h',
	})
}

// ─── moov ────────────────────────────────────────────────────────────────────

func (m *FMP4Muxer) writeMoov(w *bytes.Buffer) error {
	var moov bytes.Buffer

	// mvhd
	writeMvhd(&moov, len(m.tracks))

	// one trak per track
	for _, t := range m.tracks {
		var trak bytes.Buffer
		writeTrak(&trak, t)
		writeBox(&moov, "trak", trak.Bytes())
	}

	// mvex (needed for fragmented MP4)
	var mvex bytes.Buffer
	for _, t := range m.tracks {
		writeTrex(&mvex, t.TrackID)
	}
	writeBox(&moov, "mvex", mvex.Bytes())

	return writeBox(w, "moov", moov.Bytes())
}

func writeMvhd(w *bytes.Buffer, numTracks int) {
	// version 0 mvhd: 108 bytes total including box header
	body := make([]byte, 100)
	// timescale = 1000 ms (for the container; track timescales are in tkhd/mdhd)
	binary.BigEndian.PutUint32(body[12:], 1000)       // timescale
	binary.BigEndian.PutUint32(body[16:], 0xFFFFFFFF) // duration = unknown
	binary.BigEndian.PutUint32(body[80:], 0x00010000) // rate = 1.0
	binary.BigEndian.PutUint16(body[84:], 0x0100)     // volume = 1.0
	// matrix = identity
	binary.BigEndian.PutUint32(body[36:], 0x00010000)
	binary.BigEndian.PutUint32(body[52:], 0x00010000)
	binary.BigEndian.PutUint32(body[68:], 0x40000000)
	binary.BigEndian.PutUint32(body[96:], uint32(numTracks+1)) // next_track_id
	writeBox(w, "mvhd", body)
}

func writeTrak(w *bytes.Buffer, t *FMP4Track) {
	var tkhd bytes.Buffer
	writeTkhd(&tkhd, t)
	writeBox(w, "tkhd", tkhd.Bytes())

	var mdia bytes.Buffer
	writeMdia(&mdia, t)
	writeBox(w, "mdia", mdia.Bytes())
}

func writeTkhd(w *bytes.Buffer, t *FMP4Track) {
	body := make([]byte, 84)
	body[3] = 0x03 // flags: track enabled + in movie
	binary.BigEndian.PutUint32(body[12:], t.TrackID)
	binary.BigEndian.PutUint32(body[20:], 0xFFFFFFFF) // duration
	// matrix
	binary.BigEndian.PutUint32(body[36:], 0x00010000)
	binary.BigEndian.PutUint32(body[52:], 0x00010000)
	binary.BigEndian.PutUint32(body[68:], 0x40000000)
	if t.Type == TrackVideo {
		binary.BigEndian.PutUint32(body[76:], uint32(t.Width)<<16)  // width in 16.16
		binary.BigEndian.PutUint32(body[80:], uint32(t.Height)<<16) // height in 16.16
	}
	w.Write(body)
}

func writeMdia(w *bytes.Buffer, t *FMP4Track) {
	// mdhd
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:], t.Timescale)
	binary.BigEndian.PutUint32(mdhd[16:], 0xFFFFFFFF) // duration
	mdhd[20] = 0x55                                   // language = 'und'
	mdhd[21] = 0xC4
	writeBox(w, "mdhd", mdhd)

	// hdlr
	var hdlr bytes.Buffer
	hdlr.Write(make([]byte, 8)) // version+flags + pre_defined
	switch t.Type {
	case TrackVideo:
		hdlr.WriteString("vide")
		hdlr.Write(make([]byte, 12)) // reserved
		hdlr.WriteString("VideoHandler\x00")
	case TrackText:
		hdlr.WriteString("subt")
		hdlr.Write(make([]byte, 12))
		hdlr.WriteString("SubtitleHandler\x00")
	default:
		hdlr.WriteString("soun")
		hdlr.Write(make([]byte, 12))
		hdlr.WriteString("SoundHandler\x00")
	}
	writeBox(w, "hdlr", hdlr.Bytes())

	// minf
	var minf bytes.Buffer
	writeMinf(&minf, t)
	writeBox(w, "minf", minf.Bytes())
}

func writeMinf(w *bytes.Buffer, t *FMP4Track) {
	switch t.Type {
	case TrackVideo:
		writeBox(w, "vmhd", []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0}) // version+flags=1
	case TrackText:
		writeBox(w, "nmhd", make([]byte, 4)) // Null Media Header — version+flags only, no other fields
	default:
		writeBox(w, "smhd", make([]byte, 8))
	}

	// dinf / dref (self-contained)
	var dref bytes.Buffer
	dref.Write(make([]byte, 4))    // version+flags
	dref.Write([]byte{0, 0, 0, 1}) // entry count = 1
	// url entry
	writeBox(&dref, "url ", []byte{0, 0, 0, 1}) // flags = self-contained
	writeBox(w, "dref", dref.Bytes())
	// dinf wraps dref
	var dinf bytes.Buffer
	writeBox(&dinf, "dref", dref.Bytes()) // re-wrap
	// Actually write dinf properly
	var dinfBox bytes.Buffer
	drefData := make([]byte, 4+4+16)            // version+flags + count + url-box
	binary.BigEndian.PutUint32(drefData[4:], 1) // entry_count = 1
	// url box: 12 bytes (size+type+flags), flags=0x000001 = self-contained
	binary.BigEndian.PutUint32(drefData[8:], 12) // box size
	copy(drefData[12:], "url ")
	drefData[19] = 0x01 // self-contained flag
	writeBox(&dinfBox, "dref", drefData)
	writeBox(w, "dinf", dinfBox.Bytes())

	// stbl (sample table — empty for fragmented)
	var stbl bytes.Buffer
	writeEmptyStbl(&stbl, t)
	writeBox(w, "stbl", stbl.Bytes())
}

func writeEmptyStbl(w *bytes.Buffer, t *FMP4Track) {
	// stsd
	var stsd bytes.Buffer
	stsd.Write(make([]byte, 4))    // version+flags
	stsd.Write([]byte{0, 0, 0, 1}) // entry_count = 1
	writeSampleEntry(&stsd, t)
	writeBox(w, "stsd", stsd.Bytes())

	// stts, stsc, stsz, stco — empty (fragmented MP4 doesn't need these)
	writeBox(w, "stts", []byte{0, 0, 0, 0, 0, 0, 0, 0}) // version+flags+entry_count=0
	writeBox(w, "stsc", []byte{0, 0, 0, 0, 0, 0, 0, 0})
	writeBox(w, "stsz", []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) // +sample_size+count
	writeBox(w, "stco", []byte{0, 0, 0, 0, 0, 0, 0, 0})
}

func writeSampleEntry(w *bytes.Buffer, t *FMP4Track) {
	switch t.Type {
	case TrackVideo:
		writeAVC1(w, t)
	case TrackText:
		if t.TextFormat == "wvtt" {
			writeWVTT(w, t)
		} else {
			writeSTPP(w, t)
		}
	default:
		writeMP4A(w, t)
	}
}

// writeSTPP writes an XMLSubtitleSampleEntry (ISO/IEC 14496-30), the 'stpp'
// sample entry DASH/DVB-DASH clients expect for EBU-TT-D subtitle tracks.
func writeSTPP(w *bytes.Buffer, t *FMP4Track) {
	var stpp bytes.Buffer
	stpp.Write(make([]byte, 6)) // reserved
	stpp.Write([]byte{0, 1})    // data_reference_index = 1
	stpp.WriteString(t.Namespace)
	stpp.WriteByte(0) // namespace (null-terminated)
	stpp.WriteByte(0) // schema_location (empty, null-terminated)
	stpp.WriteByte(0) // auxiliary_mime_types (empty, null-terminated)
	writeBox(w, "stpp", stpp.Bytes())
}

// writeWVTT writes a WVTTSampleEntry (ISO/IEC 14496-30 §7.2), the 'wvtt'
// sample entry for WebVTT-in-ISOBMFF text tracks — see internal/webvtt's
// doc comment for why this format exists alongside stpp.
func writeWVTT(w *bytes.Buffer, t *FMP4Track) {
	var wvtt bytes.Buffer
	wvtt.Write(make([]byte, 6)) // reserved
	wvtt.Write([]byte{0, 1})    // data_reference_index = 1

	// vttC (WebVTTConfigurationBox): the WebVTT file header sans cues — a
	// bare "WEBVTT" line is a complete, valid header with no global
	// styling to declare.
	var vttC bytes.Buffer
	vttC.WriteString("WEBVTT\n")
	writeBox(&wvtt, "vttC", vttC.Bytes())

	writeBox(w, "wvtt", wvtt.Bytes())
}

func writeAVC1(w *bytes.Buffer, t *FMP4Track) {
	var avc1 bytes.Buffer
	avc1.Write(make([]byte, 6))  // reserved
	avc1.Write([]byte{0, 1})     // data_reference_index = 1
	avc1.Write(make([]byte, 16)) // pre_defined + reserved
	binary.Write(&avc1, binary.BigEndian, t.Width)
	binary.Write(&avc1, binary.BigEndian, t.Height)
	avc1.Write([]byte{0, 0x48, 0, 0}) // horiz resolution 72dpi
	avc1.Write([]byte{0, 0x48, 0, 0}) // vert resolution 72dpi
	avc1.Write(make([]byte, 4))       // reserved
	avc1.Write([]byte{0, 1})          // frame_count = 1
	avc1.Write(make([]byte, 32))      // compressorname
	avc1.Write([]byte{0, 0x18})       // depth = 24
	avc1.Write([]byte{0xFF, 0xFF})    // pre_defined = -1

	// avcC (AVCDecoderConfigurationRecord)
	if len(t.SPS) > 0 {
		var avcC bytes.Buffer
		avcC.WriteByte(0x01)     // configurationVersion
		avcC.WriteByte(t.SPS[1]) // AVCProfileIndication
		avcC.WriteByte(t.SPS[2]) // profile_compatibility
		avcC.WriteByte(t.SPS[3]) // AVCLevelIndication
		avcC.WriteByte(0xFF)     // lengthSizeMinusOne = 3 (4-byte NAL length)
		avcC.WriteByte(0xE1)     // numSPS = 1
		spsLen := make([]byte, 2)
		binary.BigEndian.PutUint16(spsLen, uint16(len(t.SPS)))
		avcC.Write(spsLen)
		avcC.Write(t.SPS)
		avcC.WriteByte(0x01) // numPPS = 1
		ppsLen := make([]byte, 2)
		binary.BigEndian.PutUint16(ppsLen, uint16(len(t.PPS)))
		avcC.Write(ppsLen)
		avcC.Write(t.PPS)
		writeBox(&avc1, "avcC", avcC.Bytes())
	}

	writeBox(w, "avc1", avc1.Bytes())
}

func writeMP4A(w *bytes.Buffer, t *FMP4Track) {
	var mp4a bytes.Buffer
	mp4a.Write(make([]byte, 6)) // reserved
	mp4a.Write([]byte{0, 1})    // data_reference_index
	mp4a.Write(make([]byte, 8)) // reserved
	binary.Write(&mp4a, binary.BigEndian, uint16(t.Channels))
	mp4a.Write([]byte{0, 16})                                       // sample_size = 16
	mp4a.Write(make([]byte, 4))                                     // pre_defined + reserved
	binary.Write(&mp4a, binary.BigEndian, uint32(t.SampleRate)<<16) // samplerate 16.16

	// esds (Elementary Stream Descriptor). Required for AAC (carries its
	// AudioSpecificConfig as DecoderSpecificInfo); also written for MP2
	// (objectTypeIndication 0x6B) but without a DecoderSpecificInfo tag —
	// MP2 has no equivalent out-of-band config, every frame's own header
	// already carries sample rate/channels (see mp2.go).
	if len(t.AACConfig) > 0 || t.AudioObjectType == MPEG1AudioObjectType {
		objectType := t.AudioObjectType
		if objectType == 0 {
			objectType = AACAudioObjectType
		}

		var esds bytes.Buffer
		esds.Write(make([]byte, 4)) // version+flags

		var dsiTag []byte
		if len(t.AACConfig) > 0 {
			dsiTag = append([]byte{0x05, byte(len(t.AACConfig))}, t.AACConfig...)
		}

		// ES_Descriptor
		esds.Write([]byte{0x03})                       // ES_DescrTag
		esds.WriteByte(byte(3 + 13 + len(dsiTag) + 3)) // length
		esds.Write([]byte{0x00, 0x01})                 // ES_ID
		esds.WriteByte(0x00)                           // flags
		// DecoderConfigDescriptor
		esds.Write([]byte{0x04})
		esds.WriteByte(byte(13 + len(dsiTag)))
		esds.WriteByte(objectType)                       // objectTypeIndication
		esds.WriteByte(0x15)                             // streamType = 5 (audio) + upStream=0 + reserved=1
		esds.Write([]byte{0x00, 0x00, 0x00})             // bufferSizeDB
		binary.Write(&esds, binary.BigEndian, uint32(0)) // maxBitrate
		binary.Write(&esds, binary.BigEndian, uint32(0)) // avgBitrate
		esds.Write(dsiTag)                               // DecoderSpecificInfo (empty for MP2)
		// SLConfigDescriptor
		esds.Write([]byte{0x06, 0x01, 0x02})
		writeBox(&mp4a, "esds", esds.Bytes())
	}

	writeBox(w, "mp4a", mp4a.Bytes())
}

func writeTrex(w *bytes.Buffer, trackID uint32) {
	// version+flags(4) + track_ID(4) + default_sample_description_index(4) +
	// default_sample_duration(4) + default_sample_size(4) + default_sample_flags(4) = 24 bytes.
	// The trailing three default_* fields are left 0 — every trun we write
	// always specifies explicit per-sample duration/size (flags 0x000F01),
	// so these defaults are never actually relied upon by a conforming reader.
	body := make([]byte, 24)
	binary.BigEndian.PutUint32(body[4:], trackID)
	binary.BigEndian.PutUint32(body[8:], 1) // default_sample_description_index
	writeBox(w, "trex", body)
}

// ─── moof ────────────────────────────────────────────────────────────────────

// writeMoof writes a moof box to w and returns the absolute byte offset
// (within w, counted from w's length at entry) of trun's data_offset
// placeholder field, so the caller can patch it once mdat's position is known.
func (m *FMP4Muxer) writeMoof(w *bytes.Buffer, t *FMP4Track, samples []Sample, seqNum uint32, baseDecodeTime int64) (int, error) {
	var moof bytes.Buffer

	// mfhd
	mfhd := make([]byte, 8)
	binary.BigEndian.PutUint32(mfhd[4:], seqNum)
	writeBox(&moof, "mfhd", mfhd)

	// traf
	var traf bytes.Buffer
	trafOffset, err := writeTraf(&traf, t, samples, baseDecodeTime)
	if err != nil {
		return 0, err
	}
	trafBoxStart := moof.Len()
	writeBox(&moof, "traf", traf.Bytes())
	moofOffset := trafBoxStart + 8 + trafOffset // +8 = traf's own box header

	boxStart := w.Len()
	if err := writeBox(w, "moof", moof.Bytes()); err != nil {
		return 0, err
	}
	return boxStart + 8 + moofOffset, nil // +8 = moof's own box header
}

// writeTraf writes a traf box to w and returns the absolute byte offset
// (within w, counted from w's length at entry) of trun's data_offset
// placeholder field.
func writeTraf(w *bytes.Buffer, t *FMP4Track, samples []Sample, baseDecodeTime int64) (int, error) {
	// tfhd
	var tfhd bytes.Buffer
	tfhd.Write([]byte{0x00, 0x02, 0x00, 0x00}) // flags: default-base-is-moof
	binary.Write(&tfhd, binary.BigEndian, t.TrackID)
	writeBox(w, "tfhd", tfhd.Bytes())

	// tfdt
	var tfdt bytes.Buffer
	tfdt.WriteByte(0x01)        // version 1 (64-bit baseMediaDecodeTime)
	tfdt.Write(make([]byte, 3)) // flags
	binary.Write(&tfdt, binary.BigEndian, uint64(baseDecodeTime))
	writeBox(w, "tfdt", tfdt.Bytes())

	// trun — tfhd and tfdt are already appended to w, so writeTrun's own
	// w.Len()-based offset calculation naturally lands at the right spot.
	return writeTrun(w, t, samples)
}

// writeTrun writes a trun box to w and returns the absolute byte offset
// (within w, counted from w's length at entry to writeTraf/writeTrun) of the
// data_offset field, so callers up the chain can patch it once the final
// position of the following mdat is known.
func writeTrun(w *bytes.Buffer, t *FMP4Track, samples []Sample) (int, error) {
	// flags:
	//   0x000001 = data-offset-present
	//   0x000100 = sample-duration-present
	//   0x000200 = sample-size-present
	//   0x000400 = sample-flags-present — per-sample sync/non-sync bit (see
	//              sampleFlags below). Previously omitted entirely, which
	//              left every sample defaulting to trex's
	//              default_sample_flags (always 0 = "is a sync sample" per
	//              ISO/IEC 14496-12's inverted sample_is_non_sync_sample
	//              bit) — i.e. every P-frame was incorrectly flagged as a
	//              random-access point. Undetected by this project's own
	//              tests (self-consistent box-layout checks) and by
	//              ffprobe/native players (lenient about this field), only
	//              surfaced by a strict browser MSE decoder (Chrome, via
	//              dash.js) rejecting video samples outright.
	//   0x000800 = sample-composition-time-offset-present
	flags := uint32(0x000F01) // data-offset + duration + size + flags + CTO

	var trun bytes.Buffer
	trun.WriteByte(0x01)                                                 // version 1 (signed CTO)
	trun.Write([]byte{byte(flags >> 16), byte(flags >> 8), byte(flags)}) // flags
	binary.Write(&trun, binary.BigEndian, uint32(len(samples)))

	// data_offset placeholder, patched by WriteMediaSegment once mdat's
	// position is known. Captured immediately before the write, not after —
	// version/flags/sample_count already account for trun.Len() at this point.
	localOffset := trun.Len()
	binary.Write(&trun, binary.BigEndian, int32(0))

	for _, s := range samples {
		binary.Write(&trun, binary.BigEndian, s.Duration)           // sample_duration
		binary.Write(&trun, binary.BigEndian, uint32(len(s.Data)))  // sample_size
		binary.Write(&trun, binary.BigEndian, sampleFlags(s.IsKey)) // sample_flags
		cto := int32(s.PTS - s.DTS)                                 // composition time offset (signed, version 1)
		binary.Write(&trun, binary.BigEndian, cto)
	}

	boxStart := w.Len()
	if err := writeBox(w, "trun", trun.Bytes()); err != nil {
		return 0, err
	}
	return boxStart + 8 + localOffset, nil // +8 = trun's own box header
}

// sampleFlags encodes one trun sample's ISO/IEC 14496-12 sample_flags word.
// Matches the convention real encoders (e.g. ffmpeg's movenc.c) use:
// sample_depends_on=2 ("does not depend on others") + is_non_sync_sample=0
// for a keyframe; sample_depends_on=1 ("depends on others") +
// is_non_sync_sample=1 for everything else. Getting this backwards (or
// omitting it, as this muxer used to) makes a strict decoder treat every
// inter-frame as an independently-decodable random-access point.
func sampleFlags(isKey bool) uint32 {
	if isKey {
		return 0x02000000
	}
	return 0x01010000
}

// ─── Box helper ───────────────────────────────────────────────────────────────

func writeBox(w *bytes.Buffer, boxType string, data []byte) error {
	size := uint32(8 + len(data))
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, size)
	w.Write(b)
	w.WriteString(boxType)
	_, err := w.Write(data)
	return err
}

func writeUint32(w *bytes.Buffer, v uint32) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	w.Write(b)
}
