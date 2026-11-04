package rtmp

import (
	"context"
	"encoding/base64"
	"net"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// Real libx264 SPS/PPS/IDR (64x64 baseline), as used across this repo.
const (
	playSPS = "Z0LACtkEJsBEAAADAAQAAAMAyDxImSA="
	playPPS = "aMuDyyA="
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// startPlayServer runs a real RTMP server on a loopback port.
func startPlayServer(t *testing.T, cfg *config.Config, registry *media.Registry) string {
	t.Helper()
	srv := NewServer(cfg, registry, nil, nil)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go srv.handleConn(nc)
		}
	}()
	return l.Addr().String()
}

// TestPlay_RoundTrip plays a live stream back over RTMP with this repo's own
// RTMP client (rtmp.Pull, which decodes FLV tags exactly as RTMP ingest
// does) and checks what arrives: codec config from the sequence headers,
// keyframe first, Annex-B video, raw AAC, and timestamps that keep their
// spacing. The app ("pulled") isn't a publish application: playback works
// for any live stream, e.g. one that only exists as a pull source.
func TestPlay_RoundTrip(t *testing.T) {
	registry := media.NewRegistry()
	addr := startPlayServer(t, &config.Config{}, registry)

	sps, pps := b64(t, playSPS), b64(t, playPPS)
	src := &media.Stream{Key: media.StreamKey{App: "pulled", Name: "cam"}, StartedAt: time.Now()}
	if err := registry.Register(src); err != nil {
		t.Fatal(err)
	}
	defer registry.Unregister(src)
	src.Bus.SetCodecInfo(&media.CodecInfo{
		VideoCodec: media.CodecH264, AudioCodec: media.CodecAAC, Width: 64, Height: 64,
		SPSNALu: sps, PPSNALu: pps, AACConfig: []byte{0x11, 0x90},
	})

	out := media.NewBus()
	sub := out.Subscribe(context.Background())
	defer sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go Pull(ctx, "rtmp://"+addr+"/pulled/cam", "cam", out)

	// Feed the source until the player has what it needs: a P frame and
	// audio before the first keyframe must not be sent.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		dts := int64(90000)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			key := i%10 == 3
			nal := []byte{0x41, 0x9a, 0x01, 0x02}
			if key {
				nal = []byte{0x65, 0x88, 0x84, 0x00}
			}
			// In-band SPS/PPS and an AUD must not leak into the AVCC payload.
			data := append([]byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1}, sps...)
			data = append(append(data, 0, 0, 0, 1), pps...)
			data = append(append(data, 0, 0, 1), nal...)
			src.Bus.Publish(&media.Packet{Codec: media.CodecH264, Type: media.PacketMedia, DTS: dts, PTS: dts + 3600, IsKeyframe: key, Data: data})
			src.Bus.Publish(&media.Packet{Codec: media.CodecAAC, Type: media.PacketMedia, DTS: dts, PTS: dts, Data: []byte{0x21, 0x10, 0x05}})
			dts += 1800 // 20 ms
		}
	}()

	var video, audio []*media.Packet
	deadline := time.After(8 * time.Second)
	for len(video) < 5 || len(audio) < 5 {
		select {
		case p := <-sub.Chan():
			if p.Type != media.PacketMedia && p.Type != media.PacketKeyframe {
				continue // sequence headers, as config packets
			}
			switch p.Codec {
			case media.CodecH264:
				video = append(video, p)
			case media.CodecAAC:
				audio = append(audio, p)
			}
		case <-deadline:
			t.Fatalf("got %d video, %d audio packets", len(video), len(audio))
		}
	}

	if !video[0].IsKeyframe {
		t.Error("playback didn't start at a keyframe")
	}
	if len(audio) > 0 && audio[0].DTS < video[0].DTS {
		t.Errorf("audio (dts %d) sent before the first keyframe (dts %d)", audio[0].DTS, video[0].DTS)
	}
	for _, p := range video {
		for _, nal := range splitAnnexB(p.Data) {
			if typ := nal[0] & 0x1F; typ == 7 || typ == 8 || typ == 9 {
				t.Fatalf("NAL type %d leaked into the video payload", typ)
			}
		}
		if p.PTS-p.DTS != 3600 {
			t.Errorf("composition offset %d ticks, want 3600 (40 ms)", p.PTS-p.DTS)
		}
	}
	if d := video[1].DTS - video[0].DTS; d != 1800 {
		t.Errorf("video spacing %d ticks, want 1800 (20 ms)", d)
	}
	if string(audio[0].Data) != string([]byte{0x21, 0x10, 0x05}) {
		t.Errorf("audio payload % x", audio[0].Data)
	}
	ci := out.CodecInfo()
	if ci == nil || ci.Width != 64 || string(ci.AACConfig) != string([]byte{0x11, 0x90}) {
		t.Errorf("player's codec info from the sequence headers = %+v", ci)
	}
}

// TestPlay_StreamNotFound: an unknown stream fails the play cleanly.
func TestPlay_StreamNotFound(t *testing.T) {
	addr := startPlayServer(t, &config.Config{}, media.NewRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Pull(ctx, "rtmp://"+addr+"/live/nosuch", "nosuch", media.NewBus()); err == nil {
		t.Fatal("playing a stream that doesn't exist succeeded")
	}
}

// TestPlayOnly_PublishRefused: a connection let in to play (app isn't a
// publish application) can't publish.
func TestPlayOnly_PublishRefused(t *testing.T) {
	c := &conn{appName: "pulled", playOnly: "unknown app"}
	if err := c.handlePublish(1, []AMF0Value{AMF0String("publish"), AMF0Number(0), AMF0Null{}, AMF0String("cam")}); err == nil {
		t.Fatal("publish allowed on a play-only connection")
	}
}

func TestLooksLikeEncoder(t *testing.T) {
	for _, c := range []struct {
		obj  AMF0Object
		want bool
	}{
		{AMF0Object{"type": AMF0String("nonprivate")}, true},                          // ffmpeg/OBS publish
		{AMF0Object{"flashVer": AMF0String("FMLE/3.0 (compatible; FMSc/1.0)")}, true}, // FMLE
		{AMF0Object{"flashVer": AMF0String("LNX 9,0,124,2")}, false},                  // ffmpeg/ffplay play
		{AMF0Object{"flashVer": AMF0String("WIN 32,0,0,465")}, false},                 // Flash player
		{AMF0Object{}, false},
	} {
		if got := looksLikeEncoder(c.obj); got != c.want {
			t.Errorf("looksLikeEncoder(%v) = %v, want %v", c.obj, got, c.want)
		}
	}
}
