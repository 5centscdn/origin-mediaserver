package rtsp

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

// track describes one negotiated media track parsed out of the session
// description: enough to SETUP it and depacketize its RTP payload.
type track struct {
	isVideo bool // false means audio

	payloadType uint8
	clockRate   uint32 // from a=rtpmap, or the RFC 3551 static default for well-known payload types

	codec string // "H264" | "PCMU" | "PCMA" — uppercase, matching rtpmap's encoding name

	// controlURL is where to send SETUP/where the server expects this
	// track to be addressed for PLAY range control — already resolved
	// against the session's base URL (see resolveControlURL).
	controlURL string

	// spropParameterSets holds base64 SPS/PPS from a=fmtp's
	// sprop-parameter-sets, if present — H.264-over-RTSP commonly signals
	// parameter sets out-of-band this way rather than (or in addition to)
	// sending them in-band in the RTP stream itself.
	spropParameterSets string
}

// SDP line order per RFC 4566 section 5, which pion/sdp enforces.
const (
	sessionOrder = "vosiuepcbtrzka"
	mediaOrder   = "micbka"
)

// normalizeSDP puts each section's lines in RFC 4566 order before pion/sdp
// parses them. Cameras don't always follow it, and pion rejects the whole
// description over one misplaced line. Seen on Hikvision ("HIK Media
// Server V4.83.600"): a session-level b=AS after t= and a=control, which
// ffmpeg, VLC and Nimble accept. The sort is stable, so repeated lines
// (several a=) keep their order. Lines with an unknown type go last in
// their section; empty lines are dropped.
func normalizeSDP(body []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	var out []string
	var section []string
	order := sessionOrder
	flush := func() {
		rank := func(l string) int {
			if i := strings.IndexByte(order, l[0]); i >= 0 {
				return i
			}
			return len(order)
		}
		sort.SliceStable(section, func(a, b int) bool { return rank(section[a]) < rank(section[b]) })
		out = append(out, section...)
		section = nil
	}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) < 2 || l[1] != '=' {
			continue
		}
		if l[0] == 'm' {
			flush()
			order = mediaOrder
		}
		section = append(section, l)
	}
	flush()
	return []byte(strings.Join(out, "\r\n") + "\r\n")
}

// parseSDP extracts the first H.264 video track and first PCMU/PCMA audio
// track from an SDP body. baseURL is the RTSP request URL DESCRIBE was sent
// to, used to resolve relative a=control values per RFC 2326.
func parseSDP(body []byte, baseURL string) (video, audio *track, err error) {
	var sd sdp.SessionDescription
	if err := sd.Unmarshal(normalizeSDP(body)); err != nil {
		return nil, nil, fmt.Errorf("parse sdp: %w", err)
	}

	sessionControl, _ := sd.Attribute("control")

	for _, m := range sd.MediaDescriptions {
		if len(m.MediaName.Formats) == 0 {
			continue
		}
		ptStr := m.MediaName.Formats[0]
		pt, err := strconv.Atoi(ptStr)
		if err != nil {
			continue
		}

		codec, clockRate := staticPayloadType(uint8(pt))
		if rtpmap, ok := m.Attribute("rtpmap"); ok {
			codec, clockRate = parseRTPMap(rtpmap)
		}

		mediaControl, _ := m.Attribute("control")
		controlURL := resolveControlURL(baseURL, sessionControl, mediaControl)

		t := &track{
			payloadType: uint8(pt),
			clockRate:   clockRate,
			codec:       strings.ToUpper(codec),
			controlURL:  controlURL,
		}

		switch m.MediaName.Media {
		case "video":
			if t.codec != "H264" || video != nil {
				continue // only H.264 video is supported; first video track wins
			}
			if fmtp, ok := m.Attribute("fmtp"); ok {
				t.spropParameterSets = parseFmtpSpropParameterSets(fmtp)
			}
			t.isVideo = true
			video = t
		case "audio":
			if (t.codec != "PCMU" && t.codec != "PCMA") || audio != nil {
				continue // only G.711 audio is supported; first audio track wins
			}
			audio = t
		}
	}

	if video == nil && audio == nil {
		return nil, nil, fmt.Errorf("no supported media found in SDP (need H.264 video and/or PCMU/PCMA audio)")
	}
	return video, audio, nil
}

// staticPayloadType returns the RFC 3551 well-known codec/clock-rate for
// payload types that don't require an explicit a=rtpmap line. Dynamic
// payload types (96-127) always carry their own rtpmap and are looked up
// there instead — this is only consulted as a fallback.
func staticPayloadType(pt uint8) (codec string, clockRate uint32) {
	switch pt {
	case 0:
		return "PCMU", 8000
	case 8:
		return "PCMA", 8000
	default:
		return "", 0
	}
}

// parseRTPMap parses "<payload_type> <encoding>/<clock_rate>[/<channels>]",
// e.g. "96 H264/90000" or "0 PCMU/8000".
func parseRTPMap(rtpmap string) (codec string, clockRate uint32) {
	parts := strings.SplitN(rtpmap, " ", 2)
	if len(parts) != 2 {
		return "", 0
	}
	encParts := strings.Split(parts[1], "/")
	if len(encParts) < 2 {
		return "", 0
	}
	rate, _ := strconv.ParseUint(encParts[1], 10, 32)
	return encParts[0], uint32(rate)
}

// parseFmtpSpropParameterSets extracts sprop-parameter-sets from an a=fmtp
// line, e.g. "96 packetization-mode=1;sprop-parameter-sets=Z0IAHpZUCg==,aM4wpIA=".
func parseFmtpSpropParameterSets(fmtp string) string {
	_, params, ok := strings.Cut(fmtp, " ")
	if !ok {
		return ""
	}
	for _, kv := range strings.Split(params, ";") {
		kv = strings.TrimSpace(kv)
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "sprop-parameter-sets") {
			return v
		}
	}
	return ""
}

// resolveControlURL applies RFC 2326's control-URL resolution: a
// media-level a=control that's already an absolute URL is used as-is;
// otherwise it's resolved relative to the session-level a=control (if
// present) or the original request URL (base) otherwise. A "*" control
// value (either level) means "use the base URL directly."
func resolveControlURL(base, sessionControl, mediaControl string) string {
	root := base
	if sessionControl != "" && sessionControl != "*" {
		if resolved, ok := resolveRelative(base, sessionControl); ok {
			root = resolved
		}
	}
	if mediaControl == "" || mediaControl == "*" {
		return root
	}
	if resolved, ok := resolveRelative(root, mediaControl); ok {
		return resolved
	}
	return root
}

// resolveRelative resolves a non-absolute RTSP a=control value against
// base. Deliberately NOT strict RFC 3986 relative-reference resolution
// (url.ResolveReference): that would replace base's last path segment
// entirely for a base with no trailing slash (e.g. "rtsp://host/stream1" +
// "trackID=1" => "rtsp://host/trackID=1"), but real RTSP servers/cameras
// universally expect control values to be *appended* as an additional path
// segment instead ("rtsp://host/stream1/trackID=1") — the same pragmatic
// convention every mainstream RTSP client (ffmpeg, live555, VLC) follows
// rather than the strict HTML-style semantics url.ResolveReference
// implements.
func resolveRelative(base, ref string) (string, bool) {
	if _, err := url.Parse(ref); err != nil {
		return "", false
	}
	if strings.Contains(ref, "://") {
		return ref, true // already absolute
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(ref, "/"), true
}
