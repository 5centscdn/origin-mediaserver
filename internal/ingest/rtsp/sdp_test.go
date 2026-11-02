package rtsp

import "testing"

// realCameraSDP mirrors what a typical ONVIF/RTSP IP camera actually
// offers: dynamic-payload-type H.264 video with in-band sprop-parameter-sets,
// static-payload-type PCMU audio (no rtpmap needed per RFC 3551), and both
// session-level and media-level a=control values, which is exactly the
// combination resolveControlURL exists to handle correctly.
const realCameraSDP = `v=0
o=- 0 0 IN IP4 192.168.1.100
s=Camera1
c=IN IP4 192.168.1.100
t=0 0
a=control:*
m=video 0 RTP/AVP 96
a=rtpmap:96 H264/90000
a=fmtp:96 packetization-mode=1;sprop-parameter-sets=Z0IAHpZUCg==,aM4wpIA=
a=control:trackID=1
m=audio 0 RTP/AVP 0
a=control:trackID=2
`

func TestParseSDP_RealCameraLayout(t *testing.T) {
	video, audio, err := parseSDP([]byte(realCameraSDP), "rtsp://192.168.1.100/stream1")
	if err != nil {
		t.Fatalf("parseSDP: %v", err)
	}

	if video == nil {
		t.Fatal("expected a video track")
	}
	if video.codec != "H264" || video.clockRate != 90000 || video.payloadType != 96 {
		t.Errorf("video track = %+v", video)
	}
	if video.spropParameterSets != "Z0IAHpZUCg==,aM4wpIA=" {
		t.Errorf("spropParameterSets = %q", video.spropParameterSets)
	}
	if want := "rtsp://192.168.1.100/stream1/trackID=1"; video.controlURL != want {
		t.Errorf("video controlURL = %q, want %q", video.controlURL, want)
	}

	if audio == nil {
		t.Fatal("expected an audio track")
	}
	if audio.codec != "PCMU" || audio.clockRate != 8000 || audio.payloadType != 0 {
		t.Errorf("audio track (static payload type, no rtpmap) = %+v", audio)
	}
	if want := "rtsp://192.168.1.100/stream1/trackID=2"; audio.controlURL != want {
		t.Errorf("audio controlURL = %q, want %q", audio.controlURL, want)
	}
}

func TestParseSDP_AbsoluteControlURL(t *testing.T) {
	sdp := `v=0
o=- 0 0 IN IP4 10.0.0.1
s=Cam
t=0 0
m=video 0 RTP/AVP 96
a=rtpmap:96 H264/90000
a=control:rtsp://10.0.0.1:554/stream1/track1
`
	video, _, err := parseSDP([]byte(sdp), "rtsp://10.0.0.1/stream1")
	if err != nil {
		t.Fatalf("parseSDP: %v", err)
	}
	if want := "rtsp://10.0.0.1:554/stream1/track1"; video.controlURL != want {
		t.Errorf("controlURL = %q, want %q (absolute a=control must be used verbatim)", video.controlURL, want)
	}
}

func TestParseSDP_UnsupportedCodecsIgnored(t *testing.T) {
	sdp := `v=0
o=- 0 0 IN IP4 10.0.0.1
s=Cam
t=0 0
m=video 0 RTP/AVP 97
a=rtpmap:97 H265/90000
m=audio 0 RTP/AVP 97
a=rtpmap:97 MPEG4-GENERIC/44100
`
	video, audio, err := parseSDP([]byte(sdp), "rtsp://10.0.0.1/stream1")
	if err == nil {
		t.Fatal("expected an error when no supported track is present")
	}
	if video != nil || audio != nil {
		t.Errorf("expected no tracks for unsupported codecs, got video=%+v audio=%+v", video, audio)
	}
}

func TestParseSDP_NoSuitableMedia(t *testing.T) {
	if _, _, err := parseSDP([]byte("v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=Empty\r\n"), "rtsp://x/y"); err == nil {
		t.Error("expected an error for an SDP with no media descriptions at all")
	}
}
