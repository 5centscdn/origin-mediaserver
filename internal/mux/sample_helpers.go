package mux

import "encoding/binary"

// A media.Packet carries only a timestamp, not an explicit duration — a
// sample's duration is the delta to the *next* packet's timestamp, which
// isn't known until it arrives. So video samples are held one at a time in
// a "pending" slot: each new packet completes the previous pending sample
// (now that its duration is known) and becomes the new pending sample.
//
// Audio doesn't need this: AAC-LC frames are a fixed 1024 samples each, so
// their duration is known up front from the sample rate alone.
//
// Shared by internal/hls and internal/dash, both of which mux fMP4/CMAF
// segments from the same media.Packet stream.

// AppendPendingVideoSample completes pending (if any) using dts as the end
// of its duration and appends it to samples, then returns a new pending
// sample for (dts, pts, isKey, data). The returned pending sample is not yet
// in samples — it's completed by the next call, or by
// FinalizePendingVideoSample at segment-flush time.
func AppendPendingVideoSample(samples []Sample, pending *Sample, dts, pts int64, isKey bool, data []byte) ([]Sample, *Sample) {
	if pending != nil {
		completed := *pending
		completed.Duration = uint32(dts - pending.DTS)
		samples = append(samples, completed)
	}
	return samples, &Sample{DTS: dts, PTS: pts, IsKey: isKey, Data: annexBToAVCC(data)}
}

// annexBToAVCC converts an Annex-B start-code-delimited H.264 NAL stream
// (media.Packet.Data's documented convention, used uniformly by this
// codebase's RTMP/SRT/MPEG-TS ingest and its MPEG-TS muxer) into AVCC/
// length-prefixed format: each NAL becomes a 4-byte big-endian length
// followed by the NAL bytes, with no start code — the sample format
// ISOBMFF's avc1 track requires (avcC declares lengthSizeMinusOne=3, see
// writeAVC1). Omitting this conversion (as this project did for a while)
// leaves a leading 00 00 00 01 start code in the sample; a strict decoder
// reads that as a NAL length of 1, corrupting every subsequent NAL
// boundary — self-consistent box-layout tests and lenient tools (ffprobe)
// never caught it; a real browser MSE decoder (Chrome) rejects the sample
// outright. Audio samples never go through this path, which is why only
// video decode failed.
func annexBToAVCC(data []byte) []byte {
	nals := splitAnnexBNALs(data)
	if len(nals) == 0 {
		return data // not a start-code-delimited stream at all — pass through unchanged
	}
	out := make([]byte, 0, len(data))
	var lenBuf [4]byte
	for _, nal := range nals {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(nal)))
		out = append(out, lenBuf[:]...)
		out = append(out, nal...)
	}
	return out
}

// splitAnnexBNALs splits a start-code-delimited Annex-B byte stream into
// individual NAL unit payloads (start codes stripped). Duplicated rather
// than shared — this codebase already has the identical helper duplicated
// per-package in internal/ingest/mpegts, internal/ingest/rtsp,
// internal/webrtc, and internal/dash, with no existing common package for
// it.
func splitAnnexBNALs(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	i := 0
	for i+2 < len(data) {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 {
				nals = append(nals, data[start:i])
			}
			i += 3
			start = i
			continue
		}
		if i+3 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			if start >= 0 {
				nals = append(nals, data[start:i])
			}
			i += 4
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		nals = append(nals, data[start:])
	}
	return nals
}

// aacSampleRates is the standard MPEG-4 Audio sampling-frequency-index table
// (ISO/IEC 14496-3), indexed by the 4-bit samplingFrequencyIndex encoded in
// an AudioSpecificConfig.
var aacSampleRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// ParseAACSampleRate extracts the sample rate encoded in an AAC
// AudioSpecificConfig. media.CodecInfo.SampleRate is never actually
// populated anywhere in this codebase today (decodeAudioTag only sets
// AudioCodec/AACConfig) — callers work around this by parsing the raw
// config bytes locally rather than trusting CodecInfo.SampleRate. Returns 0
// if it can't be determined.
func ParseAACSampleRate(aacConfig []byte) int {
	if len(aacConfig) < 2 {
		return 0
	}
	sfIndex := (aacConfig[0]&0x07)<<1 | (aacConfig[1] >> 7)
	if int(sfIndex) < len(aacSampleRates) {
		return aacSampleRates[sfIndex]
	}
	return 0
}

// ParseAACChannels extracts the channelConfiguration field from an AAC
// AudioSpecificConfig. Returns 0 if it can't be determined.
func ParseAACChannels(aacConfig []byte) int {
	if len(aacConfig) < 2 {
		return 0
	}
	return int((aacConfig[1] >> 3) & 0x0F)
}

// FinalizePendingVideoSample appends a still-pending sample to samples at
// segment-flush time, using fallbackDuration as its estimate since there's
// no next packet yet to compute an exact delta from. Returns the (possibly
// unchanged) samples slice and nil, since the pending slot is now empty.
func FinalizePendingVideoSample(samples []Sample, pending *Sample, fallbackDuration uint32) ([]Sample, *Sample) {
	if pending == nil {
		return samples, nil
	}
	completed := *pending
	completed.Duration = fallbackDuration
	return append(samples, completed), nil
}
