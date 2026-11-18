package streamoutputs

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/hls"
	"github.com/5centscdn/origin-mediaserver/internal/ingest/mpegts"
	"github.com/5centscdn/origin-mediaserver/internal/livestore"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

const testKID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

// fakeDRMtoday serves the CAS + CPIX endpoints with one cbcs key carrying
// Widevine and FairPlay signaling. cpixCfg is the only CPIX config it
// knows; any other gets 404, as DRMtoday answers.
func fakeDRMtoday(t *testing.T) *httptest.Server {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	pssh := append([]byte{0, 0, 0, 32, 'p', 's', 's', 'h', 0, 0, 0, 0}, []byte("\xed\xef\x8b\xa9\x79\xd6\x4a\xce\xa3\xc8\x27\xdc\xd5\x1d\x21\xed\x00\x00\x00\x00")...)
	cpix := fmt.Sprintf(`<cpix:CPIX xmlns:cpix="urn:dashif:org:cpix" xmlns:pskc="urn:ietf:params:xml:ns:keyprov:pskc">
<cpix:ContentKeyList><cpix:ContentKey kid="%[1]s" explicitIV="%[2]s" commonEncryptionScheme="cbcs"><cpix:Data><pskc:Secret><pskc:PlainValue>%[3]s</pskc:PlainValue></pskc:Secret></cpix:Data></cpix:ContentKey></cpix:ContentKeyList>
<cpix:DRMSystemList>
<cpix:DRMSystem kid="%[1]s" systemId="edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"><cpix:PSSH>%[4]s</cpix:PSSH><cpix:ContentProtectionData>%[5]s</cpix:ContentProtectionData><cpix:HLSSignalingData playlist="media">%[6]s</cpix:HLSSignalingData><cpix:HLSSignalingData playlist="master">%[7]s</cpix:HLSSignalingData></cpix:DRMSystem>
<cpix:DRMSystem kid="%[1]s" systemId="94ce86fb-07ff-4f43-adb8-93d2fa968ca2"><cpix:HLSSignalingData playlist="media">%[8]s</cpix:HLSSignalingData></cpix:DRMSystem>
</cpix:DRMSystemList></cpix:CPIX>`,
		testKID, b64("0123456789abcdef"), b64("fedcba9876543210"),
		base64.StdEncoding.EncodeToString(pssh), b64("<cenc:pssh>AAAA</cenc:pssh>"),
		b64(`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="data:text/plain;base64,AAAA",KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed",KEYFORMATVERSIONS="1"`),
		b64(`#EXT-X-SESSION-KEY:METHOD=SAMPLE-AES,URI="data:text/plain;base64,AAAA",KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed",KEYFORMATVERSIONS="1"`),
		b64(`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://drmtoday?assetId=live_cam",KEYFORMAT="com.apple.streamingkeydelivery",KEYFORMATVERSIONS="1"`))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cas/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/cas/v1/tickets/TGT-1")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /cas/v1/tickets/TGT-1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ST-1")
	})
	mux.HandleFunc("GET /frontend/cpix/v1/{m}/ingest/{cfg}/{asset}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("cfg") != "cpixcfg" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, cpix)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func drmTestSetup(t *testing.T, cpixCfg string) (context.Context, *config.Config, *db.DB, *dvr.Engine, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := fakeDRMtoday(t)
	dataDir := t.TempDir()
	database, err := db.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.ExecContext(ctx, `
		INSERT INTO applications (id, name, out_hls, out_dash, scte35_mode, ad_marker_format,
		   drm_enabled, drm_user, drm_password, drm_merchant_uuid, drm_cpix_config_id)
		VALUES ('app1', 'live', 1, 1, 'disabled', 'none', 1, 'api', 'pw', 'merchant', ?)`, cpixCfg); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Server.DataDir = dataDir
	cfg.HLS.Container = "ts" // DRM must force fMP4
	cfg.HLS.SegmentDurationS = 1
	cfg.HLS.PlaylistWindow = 10
	cfg.DASH.SegmentDurationS = 1
	cfg.DASH.PathPrefix = ""
	cfg.DRM.RequestTimeoutS = 5
	cfg.DRM.DRMtodayAuthURL = srv.URL + "/cas/v1/tickets"
	cfg.DRM.DRMtodayFEURL = srv.URL
	dvrEngine := dvr.NewEngine(cfg, database, metrics.New())
	ConfigureDRM(cfg, database, dvrEngine)
	return ctx, cfg, database, dvrEngine, dataDir
}

// TestDRM_EncryptsHLSAndDASH publishes real x264 output on a DRM app and
// checks the outputs carry encryption and the keys' signaling, and that
// unencrypted playback is refused.
func TestDRM_EncryptsHLSAndDASH(t *testing.T) {
	ctx, cfg, database, dvrEngine, dataDir := drmTestSetup(t, "cpixcfg")
	stream := &media.Stream{Key: media.StreamKey{App: "live", Name: "cam"}, Bus: media.NewBus()}

	Start(ctx, cfg, database, dvrEngine, hls.NewPlaylistRegistry(), stream, metrics.New())
	defer stream.CancelOutputs()

	if !stream.DRM().Protected || ClearPlaybackAllowed(ctx, stream) {
		t.Fatal("stream not protected right after Start")
	}
	deadline := time.Now().Add(5 * time.Second)
	for stream.DRM().Keys == nil {
		if time.Now().After(deadline) {
			t.Fatalf("keys never arrived: %+v", stream.DRM())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d := stream.DRM(); d.AssetID != "live_cam" || len(d.HLSMasterLines) != 1 {
		t.Fatalf("drm state %+v", d)
	}
	time.Sleep(100 * time.Millisecond) // packagers subscribe to the Bus

	ts, err := os.ReadFile("../ingest/mpegts/testdata/x264_bframes.ts")
	if err != nil {
		t.Fatal(err)
	}
	if err := mpegts.NewDemuxer(stream.Bus).Run(ctx, bytes.NewReader(ts)); err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Logf("demux: %v", err)
	}

	hlsDir := filepath.Join(dataDir, "hls", "live", "cam")
	dashDir := filepath.Join(dataDir, "dash", "live", "cam")
	var playlist, mpd string
	for time.Now().Before(deadline.Add(5 * time.Second)) {
		pl, ok1 := livestore.Default().ReadFile(filepath.Join(hlsDir, "index.m3u8"))
		m, ok2 := livestore.Default().ReadFile(filepath.Join(dashDir, "manifest.mpd"))
		if ok1 && ok2 && strings.Contains(string(pl), "#EXTINF") && strings.Contains(string(m), "<S ") {
			playlist, mpd = string(pl), string(m)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if playlist == "" {
		t.Fatal("no HLS segments / DASH timeline produced")
	}
	for _, want := range []string{`#EXT-X-MAP:URI="init.mp4"`, "skd://drmtoday", "urn:uuid:edef8ba9", ".m4s"} {
		if !strings.Contains(playlist, want) {
			t.Errorf("HLS playlist missing %q:\n%s", want, playlist)
		}
	}
	init, _ := livestore.Default().ReadFile(filepath.Join(hlsDir, "init.mp4"))
	for _, box := range []string{"encv", "sinf", "cbcs", "tenc", "pssh"} {
		if !bytes.Contains(init, []byte(box)) {
			t.Errorf("HLS init.mp4 has no %s box", box)
		}
	}
	seg, ok := livestore.Default().ReadFile(filepath.Join(hlsDir, "seg000000.m4s"))
	if !ok || !bytes.Contains(seg, []byte("senc")) {
		t.Error("HLS segment missing or has no senc box")
	}
	for _, want := range []string{`xmlns:cenc="urn:mpeg:cenc:2013"`, `value="cbcs"`, `cenc:default_KID="` + testKID + `"`, `urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed`, "<cenc:pssh>AAAA</cenc:pssh>"} {
		if !strings.Contains(mpd, want) {
			t.Errorf("MPD missing %q:\n%s", want, mpd)
		}
	}
	vinit, _ := livestore.Default().ReadFile(filepath.Join(dashDir, "init-video.mp4"))
	if !bytes.Contains(vinit, []byte("encv")) {
		t.Error("DASH init-video.mp4 not encrypted")
	}
	stream.CancelOutputs()
	time.Sleep(50 * time.Millisecond)
}

// TestDRM_FailsClosed: when DRMtoday refuses (unknown CPIX config) the
// stream produces no output at all and reports the error.
func TestDRM_FailsClosed(t *testing.T) {
	ctx, cfg, database, dvrEngine, dataDir := drmTestSetup(t, "wrongcfg")
	stream := &media.Stream{Key: media.StreamKey{App: "live", Name: "cam2"}, Bus: media.NewBus()}
	Start(ctx, cfg, database, dvrEngine, hls.NewPlaylistRegistry(), stream, metrics.New())
	defer stream.CancelOutputs()

	deadline := time.Now().Add(5 * time.Second)
	for stream.DRM().Error == "" {
		if time.Now().After(deadline) {
			t.Fatal("no DRM error reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stream.DRM().Error, "CPIX ingest config not found") {
		t.Errorf("error %q", stream.DRM().Error)
	}
	if livestore.Default().HasDir(filepath.Join(dataDir, "hls", "live", "cam2")) ||
		livestore.Default().HasDir(filepath.Join(dataDir, "dash", "live", "cam2")) {
		t.Error("outputs started without keys")
	}
	if ClearPlaybackAllowed(ctx, stream) {
		t.Error("clear playback allowed while keys are missing")
	}
}
