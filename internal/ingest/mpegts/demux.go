// Package mpegts implements a pure-Go MPEG-TS demuxer for ingest: it turns
// a raw MPEG-TS bitstream (from a real third-party encoder, not just this
// codebase's own mux.TSMuxer output) into media.Packets on a media.Bus —
// the ingest-side counterpart to mux.TSMuxer.
package mpegts

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

const (
	tsPacketSize = 188
	tsSyncByte   = 0x47
	pidNull      = 0x1FFF
	pidPAT       = 0x0000

	streamTypeH264   = 0x1B
	streamTypeAAC    = 0x0F
	streamTypeSCTE35 = 0x86

	// streamTypeMPEG1Audio/streamTypeMPEG2Audio (ISO/IEC 13818-1 Table 2-34)
	// both carry MPEG-1/2 Audio Layer I/II/III — confirmed live on a real
	// DVB/broadcast source (ffprobe: "Audio: mp2 ... stream_type 0x03").
	// Passthrough only (see mp2.go/media.CodecMP2) — never transcoded.
	streamTypeMPEG1Audio = 0x03
	streamTypeMPEG2Audio = 0x04

	// streamTypePrivateData (ISO/IEC 13818-1 Table 2-34) is deliberately
	// generic — DVB teletext, DVB subtitles, and AC-3 audio (among others)
	// all use it, distinguished only by which descriptor accompanies the
	// stream in the PMT's ES descriptor loop. Only recognized here when a
	// teletext_descriptor identifies a subtitle page (see
	// parseTeletextSubtitlePage) — anything else on 0x06 still falls
	// through to the "unsupported" warning, unchanged.
	streamTypePrivateData = 0x06
)

// Demuxer parses a single-program MPEG-TS stream (H.264 video + AAC/ADTS
// audio only — the two stream types mux.TSMuxer itself can produce) into
// media.Packets published on bus. Other PMT stream types are skipped with a
// warning rather than supported generically, mirroring how the rest of
// this codebase scopes down to what's actually wired end-to-end.
//
// Not safe for concurrent use — one Demuxer per stream, fed sequentially by
// its own goroutine (matching every other single-stream ingest path).
type Demuxer struct {
	bus *media.Bus
	log *slog.Logger

	pmtPID     uint16
	videoPID   uint16
	audioPID   uint16
	scte35PID  uint16
	audioCodec media.CodecType // set once, when audioPID is discovered in the PMT

	teletextPID      uint16
	teletextMagazine int
	teletextPage     int

	pesBuf map[uint16][]byte // per-PID PES reassembly buffer, keyed by PID

	sps, pps           []byte // most recent SPS/PPS, for the CodecInfo merge discipline
	hasCEA608708       bool   // sticky once true — see media.HasCEA608708
	cea708DTVCCService bool   // sticky once true — see media.UsesCEA708DTVCCService
	audioConfigSeen    bool
	audioClock         *mux.AudioClock // lazily built once the first frame's sample rate is known

	lastVideoDTS int64 // fallback PTS for an SCTE-35 command carrying no explicit splice_time

	// retimer rebuilds DTS (and repairs PTS from the bitstream's POC) while
	// the source sends video PES packets with a PTS but no DTS, which with
	// B-frames leaves DTS going backwards. See media.H264Retimer.
	retimer *media.H264Retimer
	// sawVideoDTS is sticky once any video PES carries a DTS. Muxers
	// (ffmpeg among them) leave DTS out whenever it equals PTS, which the
	// spec defines as DTS = PTS, so PTS-only PES packets in such a stream
	// are normal and must not be retimed. Only a stream that never sends
	// DTS is.
	sawVideoDTS bool

	// OnSCTE35Error, if set, is called (synchronously, on the demux
	// goroutine) whenever a section on the detected SCTE-35 PID fails to
	// parse — wiring this to a metrics counter is the caller's choice,
	// mirroring the OnPublish/OnLive callback convention used elsewhere in
	// this codebase (internal/rtmp.Server, internal/pull.Manager). nil is a
	// safe no-op; the section is always logged via d.log regardless.
	OnSCTE35Error func(err error)

	// OnBytes, if set, is called (synchronously, on the demux goroutine)
	// with the number of raw bytes read off the wire each time a chunk is
	// consumed — the ingest-side "bytes in" counter (e.g. media.Stream.
	// BytesIn) has nowhere else to be updated from, since Demuxer only holds
	// a *media.Bus, not a *media.Stream. nil is a safe no-op.
	OnBytes func(n int)
}

// NewDemuxer creates a demuxer that publishes onto bus.
func NewDemuxer(bus *media.Bus) *Demuxer {
	return &Demuxer{
		bus:    bus,
		log:    slog.With("component", "mpegts-demux"),
		pesBuf: make(map[uint16][]byte),
	}
}

// SetLogContext attaches extra key/value fields (e.g. "stream", "source",
// "mode") to this demuxer's log lines. Purely cosmetic — it never affects
// parsing — but lets a call site that knows stream identity (internal/srt,
// internal/pull) make demux-level log lines (including SCTE-35 detection)
// identifiable, without threading that identity through every method.
func (d *Demuxer) SetLogContext(kv ...any) {
	d.log = d.log.With(kv...)
}

// Run reads 188-byte TS packets from r until EOF, ctx is done, or a fatal
// read error occurs — mirrors rtmp.Pull's blocking, run-until-the-
// connection-ends contract, so internal/pull.Manager's existing supervisor
// loop (reconnect/backoff/status) works unchanged for this protocol too.
// Suited to a continuous byte stream (e.g. an HTTP response body); UDP push
// should use feedPackets per-datagram instead, since UDP has no such thing
// as a continuous stream to read from.
func (d *Demuxer) Run(ctx context.Context, r io.Reader) error {
	defer d.Flush()

	buf := make([]byte, tsPacketSize)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		if buf[0] != tsSyncByte {
			if err := d.resync(r, buf); err != nil {
				return err
			}
		}
		if d.OnBytes != nil {
			d.OnBytes(len(buf))
		}
		if err := d.handlePacket(buf); err != nil {
			d.log.Warn("ts packet error", "err", err)
		}
	}
}

// resync recovers from a lost sync byte by scanning forward for the next
// 0x47 and re-aligning buf to it. Best-effort, not a full spec-compliant
// sync algorithm (it doesn't double-check a second sync byte 188 bytes
// ahead) — real third-party senders occasionally emit stray padding bytes,
// and this is enough to recover from that without aborting the stream.
func (d *Demuxer) resync(r io.Reader, buf []byte) error {
	one := make([]byte, 1)
	for i := 1; i < tsPacketSize*4; i++ {
		if _, err := io.ReadFull(r, one); err != nil {
			return err
		}
		if one[0] == tsSyncByte {
			buf[0] = tsSyncByte
			_, err := io.ReadFull(r, buf[1:])
			return err
		}
	}
	return fmt.Errorf("mpegts: lost sync, no sync byte found within %d bytes", tsPacketSize*4)
}

// feedPackets processes a byte slice containing zero or more consecutive
// 188-byte TS packets — e.g. one UDP datagram's payload. Packets not
// starting with the sync byte are skipped rather than resynced: a UDP push
// source is expected to preserve TS packet alignment within each datagram
// (unlike Run's continuous-byte-stream case, where resync applies).
func (d *Demuxer) feedPackets(data []byte) {
	if d.OnBytes != nil {
		d.OnBytes(len(data))
	}
	for len(data) >= tsPacketSize {
		pkt := data[:tsPacketSize]
		data = data[tsPacketSize:]
		if pkt[0] != tsSyncByte {
			continue
		}
		if err := d.handlePacket(pkt); err != nil {
			d.log.Warn("ts packet error", "err", err)
		}
	}
}

func (d *Demuxer) handlePacket(pkt []byte) error {
	pusi := pkt[1]&0x40 != 0
	pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	afc := (pkt[3] >> 4) & 0x03 // adaptation_field_control

	if pid == pidNull || afc == 0 || afc == 2 {
		return nil // null padding, or no payload in this packet
	}

	payload := pkt[4:]
	if afc == 3 {
		if len(payload) == 0 {
			return nil
		}
		afLen := int(payload[0])
		if 1+afLen > len(payload) {
			return fmt.Errorf("invalid adaptation field length")
		}
		payload = payload[1+afLen:]
	}

	switch {
	case pid == pidPAT:
		if pmtPID, ok := parsePAT(payload); ok {
			d.pmtPID = pmtPID
		}
	case d.pmtPID != 0 && pid == d.pmtPID:
		d.handlePMT(payload)
	case d.videoPID != 0 && pid == d.videoPID:
		d.handlePES(pid, payload, pusi, d.flushVideo)
	case d.audioPID != 0 && pid == d.audioPID:
		d.handlePES(pid, payload, pusi, d.flushAudio)
	case d.scte35PID != 0 && pid == d.scte35PID:
		d.handleSCTE35(payload, pusi)
	case d.teletextPID != 0 && pid == d.teletextPID:
		d.handlePES(pid, payload, pusi, d.flushTeletext)
	}
	return nil
}

func (d *Demuxer) handlePMT(payload []byte) {
	streams, ok := parsePMT(payload)
	if !ok {
		return
	}
	for _, s := range streams {
		switch s.streamType {
		case streamTypeH264:
			if d.videoPID == 0 {
				d.videoPID = s.pid
				d.log.Info("mpegts: found H.264 video stream", "pid", s.pid)
			}
		case streamTypeAAC:
			if d.audioPID == 0 {
				d.audioPID = s.pid
				d.audioCodec = media.CodecAAC
				d.log.Info("mpegts: found AAC audio stream", "pid", s.pid)
			}
		case streamTypeMPEG1Audio, streamTypeMPEG2Audio:
			if d.audioPID == 0 {
				d.audioPID = s.pid
				d.audioCodec = media.CodecMP2
				d.log.Info("mpegts: found MPEG-1/2 (MP2) audio stream", "pid", s.pid)
			}
		case streamTypeSCTE35:
			if d.scte35PID == 0 {
				d.scte35PID = s.pid
				d.log.Info("mpegts: found SCTE-35 stream", "pid", s.pid)
			}
		case streamTypePrivateData:
			if d.teletextPID != 0 && s.pid == d.teletextPID {
				continue // already identified on an earlier (repeated) PMT — nothing to redo or warn about
			}
			if d.teletextPID == 0 {
				if page, ok := parseTeletextSubtitlePage(s.descriptors); ok {
					d.teletextPID = s.pid
					d.teletextMagazine = page.magazine
					d.teletextPage = page.page
					d.log.Info("mpegts: found DVB teletext subtitle stream", "pid", s.pid,
						"magazine", page.magazine, "page", fmt.Sprintf("%02x", page.page), "language", page.language)
					d.mergeTeletextCodecInfo()
					continue
				}
			}
			d.log.Warn("mpegts: unsupported PMT stream type, skipping", "stream_type", fmt.Sprintf("0x%02X", s.streamType), "pid", s.pid)
		default:
			d.log.Warn("mpegts: unsupported PMT stream type, skipping", "stream_type", fmt.Sprintf("0x%02X", s.streamType), "pid", s.pid)
		}
	}
}

// handlePES reassembles PES packets per PID: on a payload-unit-start, flush
// whatever was buffered for this PID and begin a new one; otherwise append.
// Reassembling by "next start" (rather than by PES_packet_length) handles
// both audio's typically-bounded length and video's typically-unbounded
// (zero) length uniformly.
func (d *Demuxer) handlePES(pid uint16, payload []byte, pusi bool, flush func([]byte)) {
	if pusi {
		if existing := d.pesBuf[pid]; len(existing) > 0 {
			flush(existing)
		}
		buf := make([]byte, len(payload))
		copy(buf, payload)
		d.pesBuf[pid] = buf
	} else if d.pesBuf[pid] != nil {
		d.pesBuf[pid] = append(d.pesBuf[pid], payload...)
	}
}

// handleSCTE35 parses one SCTE-35 private section (pointer_field still
// present, exactly like handlePMT's payload) and publishes it as a
// CodecSCTE35 Packet. SCTE-35 sections are MPEG-2 private sections, not
// PES — like PAT/PMT, this only supports a section that fits within a
// single TS packet (splice_info_section is almost always well under 184
// bytes; a pathological multi-descriptor section spanning multiple TS
// packets is not reassembled here — the same scope limitation
// parsePAT/parsePMT already accept).
//
// A malformed section is logged and dropped, never forwarded and never
// fatal to the stream — see scte35.Parse's doc comment.
func (d *Demuxer) handleSCTE35(payload []byte, pusi bool) {
	if !pusi {
		return
	}
	section, ok := stripSectionPointer(payload)
	if !ok {
		return
	}

	ev, err := scte35.Parse(section)
	if err != nil {
		d.log.Warn("mpegts: malformed SCTE-35 section, dropping", "err", err)
		if d.OnSCTE35Error != nil {
			d.OnSCTE35Error(err)
		}
		return
	}
	ev.PID = d.scte35PID
	ev.Source = scte35.SourceIngest // explicit, even though it's the zero value — see Event.Source's doc comment

	pts := ev.PTS
	if !ev.HasPTS {
		pts = d.lastVideoDTS // no explicit splice_time — anchor to the last known video position
	}

	d.log.Info("scte35 detected",
		"pid", fmt.Sprintf("0x%04X", d.scte35PID),
		"command", ev.CommandType,
		"event_id", ev.EventID,
		"pts", pts,
		"duration_ms", ev.BreakDurationMS,
		"out_of_network", ev.OutOfNetwork,
		"auto_return", ev.AutoReturn,
	)

	d.bus.Publish(&media.Packet{
		Type:      media.PacketMedia,
		Codec:     media.CodecSCTE35,
		DTS:       pts,
		PTS:       pts,
		Data:      section,
		SCTE35:    ev,
		WallClock: time.Now(),
	})
}

func (d *Demuxer) flushVideo(data []byte) {
	h, ok := parsePES(data)
	if !ok || len(h.payload) == 0 {
		return
	}
	pts, dts := h.pts, h.dts
	if h.hasDTS {
		d.sawVideoDTS = true
	}
	if !h.hasDTS {
		dts = pts
	}
	if !d.sawVideoDTS {
		if d.retimer == nil {
			d.retimer = media.NewH264Retimer(d.log)
			if len(d.sps) > 0 {
				d.retimer.SetSPS(d.sps)
			}
			if len(d.pps) > 0 {
				d.retimer.SetPPS(d.pps)
			}
		}
	} else if d.retimer != nil {
		// The source sends DTS after all: release what's buffered and
		// pass timestamps through from here on.
		d.publishVideo(d.retimer.Flush())
		d.retimer = nil
	}
	d.lastVideoDTS = dts

	// SEI (type 6) — e.g. CEA-608/708 closed captions carried as ITU-T
	// T.35 registered user data — always precedes the slice NAL of the
	// same access unit in decode order (real encoders emit it that way).
	// It's carried into that slice's own Packet.Data (prepended, with its
	// own start code) rather than published as a packet of its own: every
	// downstream consumer (mux.TSMuxer.WriteVideo, internal/hls.Packager,
	// internal/tsout) already treats Data as an opaque, possibly-multi-NAL
	// blob and writes it through unmodified — so this is the only place
	// in the whole pipeline that needs to change for closed captions
	// already present in the source to survive to HLS/DASH/raw-TS output.
	var pendingSEI []byte
	for _, nal := range splitAnnexB(h.payload) {
		if len(nal) == 0 {
			continue
		}
		switch nalType := nal[0] & 0x1F; nalType {
		case 7: // SPS
			d.sps = nal
			d.mergeVideoCodecInfo()
			if d.retimer != nil {
				d.retimer.SetSPS(nal)
			}
		case 8: // PPS
			d.pps = nal
			d.mergeVideoCodecInfo()
			if d.retimer != nil {
				d.retimer.SetPPS(nal)
			}
		case 6: // SEI
			pendingSEI = append(pendingSEI, withStartCode(nal)...)
			if !d.hasCEA608708 && media.HasCEA608708(nal) {
				d.hasCEA608708 = true
				d.mergeVideoCodecInfo() // no-op until SPS has been seen at least once; re-merged then too
			}
			if !d.cea708DTVCCService && media.UsesCEA708DTVCCService(nal) {
				d.cea708DTVCCService = true
				d.mergeVideoCodecInfo()
			}
		case 1, 5: // non-IDR / IDR slice
			pktType := media.PacketMedia
			if nalType == 5 {
				pktType = media.PacketKeyframe
			}
			sliceData := withStartCode(nal)
			if len(pendingSEI) > 0 {
				sliceData = append(pendingSEI, sliceData...)
				pendingSEI = nil
			}
			pkt := &media.Packet{
				Type:       pktType,
				Codec:      media.CodecH264,
				DTS:        dts,
				PTS:        pts,
				IsKeyframe: nalType == 5,
				Data:       sliceData,
				WallClock:  time.Now(),
			}
			if d.retimer != nil {
				d.publishVideo(d.retimer.Push(pkt, nal))
			} else {
				d.bus.Publish(pkt)
			}
		}
	}
}

// publishVideo publishes retimed video packets and keeps lastVideoDTS on
// the timeline downstream actually sees.
func (d *Demuxer) publishVideo(pkts []*media.Packet) {
	for _, p := range pkts {
		d.lastVideoDTS = p.DTS
		d.bus.Publish(p)
	}
}

func (d *Demuxer) flushAudio(data []byte) {
	h, ok := parsePES(data)
	if !ok || len(h.payload) == 0 {
		return
	}

	if d.audioCodec == media.CodecMP2 {
		d.flushMP2Audio(h)
		return
	}

	frames := splitADTS(h.payload)

	// pts tracks each frame's own timestamp: the PES packet itself carries
	// only one PTS, but real encoders routinely batch several 1024-sample
	// AAC frames into a single PES to cut overhead (confirmed live against
	// real ffmpeg output — a single PES commonly held 14+ frames). Stamping
	// every split-out frame with the same PES-level PTS previously produced
	// many consecutive audio packets with an identical, non-advancing
	// timestamp — real demuxers/decoders downstream saw this as
	// non-monotonic DTS and outright decode errors. mux.AudioClock (built
	// for the exact same "synthesize a fixed-frame-size codec's per-frame
	// timestamp without drift" problem on the output/muxing side) gives
	// each split frame after the first its own correctly-advanced PTS,
	// while still resyncing to the PES's real PTS at the start of every
	// batch.
	pts := h.pts
	for _, f := range frames {
		if !d.audioConfigSeen {
			d.mergeAudioCodecInfo(f)
			d.audioConfigSeen = true
		}
		if d.audioClock == nil {
			sampleRate := mux.ParseAACSampleRate(buildAudioSpecificConfig(f.audioObjectType, f.sfIndex, f.channelConfig))
			if sampleRate == 0 {
				sampleRate = 44100
			}
			d.audioClock = mux.NewAudioClock(sampleRate, 1024)
		}
		duration := d.audioClock.NextDuration(pts)
		d.bus.Publish(&media.Packet{
			Type:      media.PacketMedia,
			Codec:     media.CodecAAC,
			DTS:       pts,
			PTS:       pts,
			Data:      f.data,
			WallClock: time.Now(),
		})
		pts += int64(duration)
	}
}

// flushMP2Audio mirrors flushAudio's per-frame PTS synthesis (see its doc
// comment) for MPEG-1/2 Audio Layer II: 1152 samples/frame instead of AAC's
// 1024, and frame data is passed through byte-for-byte (header included —
// see mp2Frame's doc comment), never transcoded.
func (d *Demuxer) flushMP2Audio(h pesHeader) {
	frames := splitMP2(h.payload)

	pts := h.pts
	for _, f := range frames {
		if !d.audioConfigSeen {
			d.mergeMP2CodecInfo(f)
			d.audioConfigSeen = true
		}
		if d.audioClock == nil {
			d.audioClock = mux.NewAudioClock(f.sampleRate, 1152)
		}
		duration := d.audioClock.NextDuration(pts)
		d.bus.Publish(&media.Packet{
			Type:      media.PacketMedia,
			Codec:     media.CodecMP2,
			DTS:       pts,
			PTS:       pts,
			Data:      f.data,
			WallClock: time.Now(),
		})
		pts += int64(duration)
	}
}

// dataUnitEBUTeletextNonSubtitle/dataUnitEBUTeletextSubtitle (ETSI EN 300
// 472 table 4) are the only two data_unit_id values a stream identified by
// a teletext_descriptor may use (0xFF, stuffing, is skipped as a no-op).
// Both are forwarded identically: the subtitle PAGE is what internal/
// teletext.Decoder filters for (via magazine+page, not this transport-level
// distinction), and a genuine subtitle page's rows are sometimes carried in
// "non-subtitle" data_units by real encoders in practice.
const (
	dataUnitEBUTeletextNonSubtitle = 0x02
	dataUnitEBUTeletextSubtitle    = 0x03
)

// flushTeletext parses one reassembled PES packet on the teletext PID
// (ETSI EN 300 472 §4.3's PES_data_field: data_identifier, then a sequence
// of data_unit_id + data_unit_length + data_field) and publishes each real
// EBU teletext data_field as its own media.Packet — internal/teletext
// (invoked from internal/dash.Packager, mirroring how CEA-608/708 SEI bytes
// flow from ingest to internal/captiondecode) does the actual Hamming/row
// decode; this function only extracts and forwards raw bytes.
func (d *Demuxer) flushTeletext(data []byte) {
	h, ok := parsePES(data)
	if !ok || len(h.payload) < 1 {
		return
	}

	pos := 1 // skip data_identifier
	for pos+2 <= len(h.payload) {
		dataUnitID := h.payload[pos]
		dataUnitLength := int(h.payload[pos+1])
		pos += 2
		if pos+dataUnitLength > len(h.payload) {
			break // truncated — drop and stop, matching this demuxer's tolerance elsewhere
		}
		dataField := h.payload[pos : pos+dataUnitLength]
		pos += dataUnitLength

		if dataUnitID != dataUnitEBUTeletextNonSubtitle && dataUnitID != dataUnitEBUTeletextSubtitle {
			continue // stuffing (0xFF) or reserved — nothing to forward
		}
		if len(dataField) != 44 {
			continue // ETSI EN 300 472: data_unit_length is always 0x2C (44) for EBU Teletext
		}
		d.bus.Publish(&media.Packet{
			Type:      media.PacketMedia,
			Codec:     media.CodecDVBTeletext,
			DTS:       h.pts,
			PTS:       h.pts,
			Data:      dataField,
			WallClock: time.Now(),
		})
	}
}

// mergeTeletextCodecInfo folds the PMT-discovered subtitle page's magazine/
// page into CodecInfo, preserving whatever video/audio have already set —
// same merge discipline as mergeVideoCodecInfo/mergeAudioCodecInfo.
func (d *Demuxer) mergeTeletextCodecInfo() {
	ci := d.bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.HasDVBTeletextSubtitle = true
	merged.TeletextMagazine = d.teletextMagazine
	merged.TeletextPage = d.teletextPage
	d.bus.SetCodecInfo(&merged)
}

// Flush processes any still-buffered PES packet for the video/audio PIDs.
// Call once after the last TS packet of a stream: handlePES only flushes a
// PID's buffered PES packet when the NEXT one starts, which never happens
// for the very last access unit before a stream ends.
func (d *Demuxer) Flush() {
	if buf := d.pesBuf[d.videoPID]; len(buf) > 0 {
		d.flushVideo(buf)
		delete(d.pesBuf, d.videoPID)
	}
	if d.retimer != nil {
		d.publishVideo(d.retimer.Flush())
	}
	if buf := d.pesBuf[d.audioPID]; len(buf) > 0 {
		d.flushAudio(buf)
		delete(d.pesBuf, d.audioPID)
	}
	if buf := d.pesBuf[d.teletextPID]; len(buf) > 0 {
		d.flushTeletext(buf)
		delete(d.pesBuf, d.teletextPID)
	}
}

// mergeVideoCodecInfo folds newly-seen SPS/PPS into the stream's CodecInfo,
// preserving whatever the audio side has already set — same merge
// discipline as internal/webrtc's PublishSession.mergeVideoCodecInfo.
func (d *Demuxer) mergeVideoCodecInfo() {
	if len(d.sps) == 0 {
		return
	}
	ci := d.bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.VideoCodec = media.CodecH264
	merged.SPSNALu = d.sps
	merged.Width, merged.Height = media.ParseSPSResolution(d.sps)
	if len(d.sps) >= 4 {
		merged.Profile = d.sps[1]
		merged.ProfileCompatibility = d.sps[2]
		merged.Level = d.sps[3]
	}
	if len(d.pps) > 0 {
		merged.PPSNALu = d.pps
	}
	merged.HasCEA608708 = d.hasCEA608708
	merged.CEA708DTVCCService = d.cea708DTVCCService
	d.bus.SetCodecInfo(&merged)
}

// mergeAudioCodecInfo folds ADTS's inline profile/sample-rate/channel
// fields into CodecInfo as a synthesized AudioSpecificConfig, preserving
// whatever the video side has already set. Deliberately does NOT populate
// CodecInfo.SampleRate/Channels — those fields are never populated by any
// ingest path in this codebase (see mux.ParseAACSampleRate's doc comment);
// callers already parse sample rate/channels out of AACConfig on demand,
// and populating them here only for MPEG-TS would be a one-off
// inconsistency, not a fix.
func (d *Demuxer) mergeAudioCodecInfo(f adtsFrame) {
	ci := d.bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.AudioCodec = media.CodecAAC
	merged.AACConfig = buildAudioSpecificConfig(f.audioObjectType, f.sfIndex, f.channelConfig)
	d.bus.SetCodecInfo(&merged)
}

// mergeMP2CodecInfo mirrors mergeAudioCodecInfo for MP2: unlike AAC, MP2 has
// no separate out-of-band AudioSpecificConfig-style blob to synthesize —
// every frame's own header already carries sample rate/channel count, so
// CodecInfo.SampleRate/Channels are populated directly here instead (the
// AAC path deliberately avoids populating those two fields — see its own
// doc comment — precisely because AAC already has AACConfig to carry the
// same information; MP2 has no such alternative).
func (d *Demuxer) mergeMP2CodecInfo(f mp2Frame) {
	ci := d.bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.AudioCodec = media.CodecMP2
	merged.SampleRate = f.sampleRate
	merged.Channels = f.channels
	d.bus.SetCodecInfo(&merged)
}
