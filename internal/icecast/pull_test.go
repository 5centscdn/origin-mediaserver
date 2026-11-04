package icecast

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// TestPull plays real MP3 and ADTS AAC streams over HTTP, as an Icecast
// server sends them: with an audio Content-Type, and with a generic one
// that leaves the codec to be sniffed.
func TestPull(t *testing.T) {
	mp3, err := base64.StdEncoding.DecodeString(fixtureMP3)
	if err != nil {
		t.Fatal(err)
	}
	var aac []byte
	for i := 0; i < 20; i++ {
		aac = append(aac, buildTestADTS(bytes.Repeat([]byte{0x21}, 50), 4, 2)...)
	}

	cases := []struct {
		name, contentType string
		body              []byte
		want              media.CodecType
	}{
		{"mp3 audio/mpeg", "audio/mpeg", mp3, media.CodecMP3},
		{"mp3 sniffed", "application/octet-stream", mp3, media.CodecMP3},
		{"aac audio/aacp", "audio/aacp", aac, media.CodecAAC},
		{"aac sniffed", "", aac, media.CodecAAC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				} else {
					w.Header()["Content-Type"] = nil // no sniffing by net/http either
				}
				w.Write(tc.body)
			}))
			defer srv.Close()

			bus := media.NewBus()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sub := bus.Subscribe(ctx)
			defer sub.Close()
			done := make(chan error, 1)
			go func() { done <- Pull(ctx, srv.URL+"/broadwavehigh.mp3", bus) }()

			n := 0
			for n < 3 {
				select {
				case p := <-sub.Chan():
					if p.Codec != tc.want {
						t.Fatalf("codec %v, want %v", p.Codec, tc.want)
					}
					n++
				case err := <-done:
					if n == 0 {
						t.Fatalf("Pull ended with no audio: %v", err)
					}
					return
				case <-ctx.Done():
					t.Fatal("no audio packets")
				}
			}
			if ci := bus.CodecInfo(); ci == nil || ci.AudioCodec != tc.want {
				t.Errorf("CodecInfo = %+v", ci)
			}
		})
	}
}

func TestPullRejectsNonAudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not a stream</html>"))
	}))
	defer srv.Close()
	if err := Pull(context.Background(), srv.URL, media.NewBus()); err == nil {
		t.Fatal("HTML page accepted as an audio stream")
	}
}
