package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
)

// TestHandlePlaylist_ClosedCaptionsSignaling covers the HLS-facing half of
// closed-caption support: once a stream's CodecInfo reports CEA-608/708
// captions are present (media.HasCEA608708, detected from an in-band SEI
// NAL — see internal/ingest/mpegts's regression test for that half), the
// master playlist must declare #EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS and tag
// the variant with CLOSED-CAPTIONS="cc1" (not just silently pass the SEI
// data through with no player-facing signal at all). A stream confirmed to
// have NO captions must instead get the explicit CLOSED-CAPTIONS=NONE
// RFC 8216 requires for "known to have none," and a stream whose CodecInfo
// isn't populated yet must get neither (unknown, not "none").
func TestHandlePlaylist_ClosedCaptionsSignaling(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	registry := media.NewRegistry()
	m := metrics.New()
	scte35Mgr := scte35ctrl.NewManager(ctx, database, registry, m)
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-at-least-32-bytes-long!!"
	cfg.Server.DataDir = t.TempDir()

	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m, scte35Mgr)

	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	sps := []byte{0x67, 0x42, 0xC0, 0x1E}
	pps := []byte{0x68, 0xCE}

	t.Run("captions present", func(t *testing.T) {
		stream := &media.Stream{Key: media.StreamKey{App: "app1", Name: "cc"}, Bus: media.NewBus()}
		if err := registry.Register(stream); err != nil {
			t.Fatal(err)
		}
		stream.Bus.SetCodecInfo(&media.CodecInfo{
			VideoCodec: media.CodecH264, AudioCodec: media.CodecAAC,
			SPSNALu: sps, PPSNALu: pps, AACConfig: []byte{0x12, 0x10},
			HasCEA608708: true,
		})

		body := get("/app1/cc/playlist.m3u8")
		if !strings.Contains(body, `#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS`) {
			t.Errorf("missing #EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS:\n%s", body)
		}
		if !strings.Contains(body, `INSTREAM-ID="CC1"`) {
			t.Errorf("missing INSTREAM-ID=\"CC1\":\n%s", body)
		}
		if !strings.Contains(body, `CLOSED-CAPTIONS="cc1"`) {
			t.Errorf("STREAM-INF missing CLOSED-CAPTIONS=\"cc1\":\n%s", body)
		}
	})

	t.Run("CEA-708 DTVCC service captions", func(t *testing.T) {
		// Real encoders commonly send CEA-708 DTVCC service data, not
		// legacy 608 field pairs, even though HasCEA608708 is true either
		// way — see media.UsesCEA708DTVCCService's doc comment. HLS must
		// signal this with the "SERVICE1" INSTREAM-ID convention, not
		// "CC1", or a compliant player looks on the wrong channel and
		// renders nothing despite the caption bytes being present.
		stream := &media.Stream{Key: media.StreamKey{App: "app1", Name: "cc708"}, Bus: media.NewBus()}
		if err := registry.Register(stream); err != nil {
			t.Fatal(err)
		}
		stream.Bus.SetCodecInfo(&media.CodecInfo{
			VideoCodec: media.CodecH264, AudioCodec: media.CodecAAC,
			SPSNALu: sps, PPSNALu: pps, AACConfig: []byte{0x12, 0x10},
			HasCEA608708:       true,
			CEA708DTVCCService: true,
		})

		body := get("/app1/cc708/playlist.m3u8")
		if !strings.Contains(body, `#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS`) {
			t.Errorf("missing #EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS:\n%s", body)
		}
		if !strings.Contains(body, `INSTREAM-ID="SERVICE1"`) {
			t.Errorf("missing INSTREAM-ID=\"SERVICE1\" for a CEA-708 DTVCC stream:\n%s", body)
		}
		if strings.Contains(body, `INSTREAM-ID="CC1"`) {
			t.Errorf("wrongly signaled INSTREAM-ID=\"CC1\" (608 convention) for a CEA-708 DTVCC stream:\n%s", body)
		}
		if !strings.Contains(body, `CLOSED-CAPTIONS="cc708"`) {
			t.Errorf("STREAM-INF missing CLOSED-CAPTIONS=\"cc708\":\n%s", body)
		}
	})

	t.Run("confirmed no captions", func(t *testing.T) {
		stream := &media.Stream{Key: media.StreamKey{App: "app1", Name: "nocc"}, Bus: media.NewBus()}
		if err := registry.Register(stream); err != nil {
			t.Fatal(err)
		}
		stream.Bus.SetCodecInfo(&media.CodecInfo{
			VideoCodec: media.CodecH264, AudioCodec: media.CodecAAC,
			SPSNALu: sps, PPSNALu: pps, AACConfig: []byte{0x12, 0x10},
			HasCEA608708: false,
		})

		body := get("/app1/nocc/playlist.m3u8")
		if strings.Contains(body, `TYPE=CLOSED-CAPTIONS`) {
			t.Errorf("unexpected #EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS for a stream confirmed to have none:\n%s", body)
		}
		if !strings.Contains(body, `CLOSED-CAPTIONS=NONE`) {
			t.Errorf("STREAM-INF missing CLOSED-CAPTIONS=NONE for a stream confirmed to have none:\n%s", body)
		}
	})

	t.Run("codec info not yet known", func(t *testing.T) {
		stream := &media.Stream{Key: media.StreamKey{App: "app1", Name: "unknown"}, Bus: media.NewBus()}
		if err := registry.Register(stream); err != nil {
			t.Fatal(err)
		}
		// No SetCodecInfo call — mirrors a stream that has just registered
		// and hasn't produced a sequence header yet.

		body := get("/app1/unknown/playlist.m3u8")
		if strings.Contains(body, `CLOSED-CAPTIONS`) {
			t.Errorf("CLOSED-CAPTIONS attribute emitted before CodecInfo is known (should be omitted, not NONE):\n%s", body)
		}
	})
}
