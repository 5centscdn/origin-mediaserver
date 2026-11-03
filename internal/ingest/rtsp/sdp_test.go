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

// A Hikvision camera ("HIK Media Server V4.83.600") puts the session b=
// after t= and a=, out of RFC 4566 order; pion/sdp rejected it ("syntax
// error at pos 172") and the pull never started. Captured from a real
// DS-series NVR channel; ffmpeg plays it.
func TestParseSDP_HikvisionOutOfOrder(t *testing.T) {
	body := "v=0\r\n" +
		"o=- 1109162014219182 0 IN IP4 0.0.0.0\r\n" +
		"s=HIK Media Server V4.83.600\r\n" +
		"i=HIK Media Server Session Description : standard\r\n" +
		"e=NONE\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"t=0 0\r\n" +
		"a=control:*\r\n" +
		"b=AS:1546\r\n" +
		"a=range:npt=now-\r\n" +
		"m=video 0 RTP/AVP 96\r\n" +
		"i=Video Media\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=fmtp:96 profile-level-id=4D0014;packetization-mode=0;sprop-parameter-sets=Z2QAIKwbGoDwCJoQAABwgAAV+QBA,aO48sA==\r\n" +
		"a=control:trackID=video\r\n" +
		"b=AS:1536\r\n" +
		"m=audio 0 RTP/AVP 0\r\n" +
		"i=Audio Media\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=control:trackID=audio\r\n" +
		"b=AS:10\r\n" +
		"a=Media_header:MEDIAINFO=494D4B48020100000400000110710110401F000000FA000000000000000000000000000000000000;\r\n" +
		"a=appversion:1.0\r\n"
	base := "rtsp://203.0.113.5:554/Streaming/Channels/301"
	video, audio, err := parseSDP([]byte(body), base)
	if err != nil {
		t.Fatalf("parseSDP: %v", err)
	}
	if video == nil || video.codec != "H264" || video.controlURL != base+"/trackID=video" || video.spropParameterSets == "" {
		t.Errorf("video track = %+v", video)
	}
	if audio == nil || audio.codec != "PCMU" || audio.clockRate != 8000 || audio.controlURL != base+"/trackID=audio" {
		t.Errorf("audio track = %+v", audio)
	}
}
