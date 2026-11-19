package drm

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

const (
	kidAudio = "1b4ab5d4-0a52-4c1b-9b39-7d1b7d0e0a01"
	kidSD    = "1b4ab5d4-0a52-4c1b-9b39-7d1b7d0e0a02"
	kidHD    = "1b4ab5d4-0a52-4c1b-9b39-7d1b7d0e0a03"
)

// sampleCPIX follows the shape of DRMtoday's CPIX 2.3 ingest response
// (Unified Streaming's documented sample): cpix/pskc prefixes, explicitIV,
// one DRMSystem per kid and system, usage rules by pixel count.
func sampleCPIX() string {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	iv := base64.StdEncoding.EncodeToString([]byte("fedcba9876543210"))
	var keys, systems strings.Builder
	for _, kid := range []string{kidAudio, kidSD, kidHD} {
		fmt.Fprintf(&keys, `<cpix:ContentKey kid="%s" explicitIV="%s" commonEncryptionScheme="cbcs"><cpix:Data><pskc:Secret><pskc:PlainValue>%s</pskc:PlainValue></pskc:Secret></cpix:Data></cpix:ContentKey>`, kid, iv, key)
		fmt.Fprintf(&systems, `<cpix:DRMSystem kid="%s" systemId="%s"><cpix:PSSH>%s</cpix:PSSH><cpix:ContentProtectionData>%s</cpix:ContentProtectionData><cpix:HLSSignalingData playlist="media">%s</cpix:HLSSignalingData><cpix:HLSSignalingData playlist="master">%s</cpix:HLSSignalingData></cpix:DRMSystem>`,
			kid, strings.ToUpper(SystemWidevine), b64("\x00\x00\x00\x20psshWV"), b64(`<cenc:pssh>AAAA</cenc:pssh>`),
			b64(`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="data:text/plain;base64,AAAA",KEYID=0x`+kid[:8]+`,KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed",KEYFORMATVERSIONS="1"`),
			b64(`#EXT-X-SESSION-KEY:METHOD=SAMPLE-AES,URI="data:text/plain;base64,AAAA",KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"`))
		fmt.Fprintf(&systems, `<cpix:DRMSystem kid="%s" systemId="%s"><cpix:HLSSignalingData playlist="media">%s</cpix:HLSSignalingData></cpix:DRMSystem>`,
			kid, SystemFairPlay, b64(`METHOD=SAMPLE-AES,URI="skd://drmtoday?assetId=a&keyId=`+kid+`",KEYFORMAT="com.apple.streamingkeydelivery",KEYFORMATVERSIONS="1"`))
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<cpix:CPIX xmlns:cpix="urn:dashif:org:cpix" xmlns:pskc="urn:ietf:params:xml:ns:keyprov:pskc" contentId="asset1" version="2.3">
<cpix:ContentKeyList>` + keys.String() + `</cpix:ContentKeyList>
<cpix:DRMSystemList>` + systems.String() + `</cpix:DRMSystemList>
<cpix:ContentKeyUsageRuleList>
<cpix:ContentKeyUsageRule kid="` + kidAudio + `" intendedTrackType="AUDIO"><cpix:AudioFilter/></cpix:ContentKeyUsageRule>
<cpix:ContentKeyUsageRule kid="` + kidSD + `" intendedTrackType="SD"><cpix:VideoFilter maxPixels="442368"/></cpix:ContentKeyUsageRule>
<cpix:ContentKeyUsageRule kid="` + kidHD + `" intendedTrackType="HD"><cpix:VideoFilter minPixels="442369"/></cpix:ContentKeyUsageRule>
</cpix:ContentKeyUsageRuleList>
</cpix:CPIX>`
}

func TestParseCPIX(t *testing.T) {
	ks, err := ParseCPIX([]byte(sampleCPIX()))
	if err != nil {
		t.Fatal(err)
	}
	if len(ks.Keys) != 3 || len(ks.Systems) != 6 {
		t.Fatalf("keys=%d systems=%d", len(ks.Keys), len(ks.Systems))
	}
	if got := ks.AudioKey().KIDString(); got != kidAudio {
		t.Errorf("audio key %s", got)
	}
	if got := ks.VideoKey(640, 360).KIDString(); got != kidSD {
		t.Errorf("SD key %s", got)
	}
	if got := ks.VideoKey(1920, 1080).KIDString(); got != kidHD {
		t.Errorf("HD key %s", got)
	}
	if got := ks.VideoKey(0, 0).KIDString(); got != kidSD {
		t.Errorf("unknown-resolution key %s, want first video rule", got)
	}
	k := ks.VideoKey(1920, 1080)
	if string(k.Key[:]) != "0123456789abcdef" || string(k.IV[:]) != "fedcba9876543210" {
		t.Errorf("key/iv not decoded")
	}
	media := ks.HLSMediaLines(k.KID)
	if len(media) != 2 || !strings.HasPrefix(media[0], "#EXT-X-KEY:") || !strings.HasPrefix(media[1], "#EXT-X-KEY:") {
		t.Fatalf("media lines %q", media)
	}
	// Widevine, then FairPlay (systemLess), whatever the CPIX order.
	if !strings.Contains(media[1], "skd://") {
		t.Errorf("want FairPlay last, got %q", media[1])
	}
	if n := len(ks.PSSHFor(k.KID)); n != 1 {
		t.Errorf("pssh boxes %d, want 1 (FairPlay has none)", n)
	}
	if m := ks.HLSMasterLines(); len(m) != 1 {
		t.Errorf("master lines %d, want identical lines de-duplicated to 1", len(m))
	}
	sys := ks.SystemsFor(k.KID)
	// Widevine first: Shaka Player only uses the first HLS key tag.
	if sys[0].SystemID != SystemWidevine || sys[0].ContentProtection != "<cenc:pssh>AAAA</cenc:pssh>" {
		t.Errorf("widevine system %+v", sys[0])
	}
	if sys[1].SystemID != SystemFairPlay {
		t.Errorf("second system %s, want FairPlay", sys[1].SystemID)
	}
	if !strings.Contains(media[0], "edef8ba9") {
		t.Errorf("first HLS key line is not Widevine: %q", media[0])
	}
}

func TestParseCPIX_RejectsCENCAndEncrypted(t *testing.T) {
	if _, err := ParseCPIX([]byte(strings.Replace(sampleCPIX(), `"cbcs"`, `"cenc"`, 1))); err == nil {
		t.Error("cenc scheme accepted")
	}
	doc := `<CPIX><ContentKeyList><ContentKey kid="` + kidAudio + `"><Data><Secret><EncryptedValue>x</EncryptedValue></Secret></Data></ContentKey></ContentKeyList></CPIX>`
	if _, err := ParseCPIX([]byte(doc)); err == nil {
		t.Error("encrypted key accepted")
	}
}

func TestParseCPIX_NoIVUsesKID(t *testing.T) {
	doc := `<CPIX><ContentKeyList><ContentKey kid="` + kidAudio + `"><Data><Secret><PlainValue>` +
		base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")) + `</PlainValue></Secret></Data></ContentKey></ContentKeyList></CPIX>`
	ks, err := ParseCPIX([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if ks.Keys[0].IV != ks.Keys[0].KID {
		t.Error("IV should default to KID")
	}
	if ks.VideoKey(1, 1).KID != ks.AudioKey().KID {
		t.Error("single key should serve audio and video")
	}
}

// fakeDRMtoday serves CAS + CPIX like DRMtoday.
type fakeDRMtoday struct {
	srv        *httptest.Server
	logins     atomic.Int32
	cpixCalls  atomic.Int32
	expireOnce atomic.Bool // reject the first service-ticket request (expired TGT)
	mu         sync.Mutex
	tickets    map[string]string // ST → service
}

func newFake(t *testing.T) *fakeDRMtoday {
	f := &fakeDRMtoday{tickets: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cas/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("username") != "api" || r.Form.Get("password") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := f.logins.Add(1)
		w.Header().Set("Location", fmt.Sprintf("/cas/v1/tickets/TGT-%d", n))
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /cas/v1/tickets/{tgt}", func(w http.ResponseWriter, r *http.Request) {
		if f.expireOnce.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		r.ParseForm()
		st := "ST-" + r.PathValue("tgt")
		f.mu.Lock()
		f.tickets[st] = r.Form.Get("service")
		f.mu.Unlock()
		fmt.Fprint(w, st)
	})
	mux.HandleFunc("GET /frontend/cpix/v1/{merchant}/ingest/{cfg}/{asset}", func(w http.ResponseWriter, r *http.Request) {
		f.cpixCalls.Add(1)
		f.mu.Lock()
		svc, ok := f.tickets[r.URL.Query().Get("ticket")]
		delete(f.tickets, r.URL.Query().Get("ticket"))
		f.mu.Unlock()
		want := f.srv.URL + r.URL.Path
		if !ok || svc != want {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.PathValue("cfg") != "cfg1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("x-signaling-alg") != "cbcs" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		fmt.Fprint(w, sampleCPIX())
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDRMtoday) account() Account {
	return Account{User: "api", Password: "secret", MerchantUUID: "m1", CPIXConfigID: "cfg1",
		AuthURL: f.srv.URL + "/cas/v1/tickets", FEURL: f.srv.URL}
}

func TestClientFetch(t *testing.T) {
	f := newFake(t)
	c := NewClient(5 * time.Second)
	ctx := context.Background()
	ks, err := c.Keys(ctx, f.account(), "live_test")
	if err != nil {
		t.Fatal(err)
	}
	if ks.AssetID != "live_test" || len(ks.Keys) != 3 {
		t.Fatalf("got %+v", ks)
	}
	// TGT reused.
	if _, err := c.Keys(ctx, f.account(), "live_test"); err != nil {
		t.Fatal(err)
	}
	if n := f.logins.Load(); n != 1 {
		t.Errorf("logins %d, want 1", n)
	}
	// Expired TGT → one fresh login.
	f.expireOnce.Store(true)
	if _, err := c.Keys(ctx, f.account(), "live_test"); err != nil {
		t.Fatal(err)
	}
	if n := f.logins.Load(); n != 2 {
		t.Errorf("logins %d, want 2 after expiry", n)
	}
}

func TestClientErrors(t *testing.T) {
	f := newFake(t)
	c := NewClient(5 * time.Second)
	a := f.account()
	a.Password = "wrong"
	_, err := c.Keys(context.Background(), a, "x")
	if err == nil || !strings.Contains(err.Error(), "wrong API user or password") {
		t.Errorf("bad password: %v", err)
	}
	a = f.account()
	a.CPIXConfigID = "nope"
	_, err = c.Keys(context.Background(), a, "x")
	if err == nil || !strings.Contains(err.Error(), "CPIX ingest config not found") {
		t.Errorf("bad config: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "secret") {
		t.Error("password leaked into error")
	}
	if _, err := c.Keys(context.Background(), Account{}, "x"); err == nil {
		t.Error("empty account accepted")
	}
}

func TestProviderSharesAndCaches(t *testing.T) {
	f := newFake(t)
	p := NewProvider(NewClient(5 * time.Second))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Keys(context.Background(), f.account(), "grp"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.cpixCalls.Load(); n != 1 {
		t.Errorf("cpix calls %d, want 1", n)
	}
	if _, ok := p.Cached(f.account(), "grp"); !ok {
		t.Error("not cached")
	}
	p.Forget(f.account())
	if _, ok := p.Cached(f.account(), "grp"); ok {
		t.Error("Forget kept the entry")
	}
}

func TestAssetID(t *testing.T) {
	cases := []struct{ tmpl, app, stream, group, want string }{
		{"", "live", "test2", "test", "live_test"},
		{"", "live", "cam", "", "live_cam"},
		{"{stream}", "live", "test2", "test", "test2"},
		{"movie", "live", "x", "", "movie"},
	}
	for _, c := range cases {
		if got := AssetID(c.tmpl, c.app, c.stream, c.group); got != c.want {
			t.Errorf("AssetID(%q,%q,%q,%q)=%q want %q", c.tmpl, c.app, c.stream, c.group, got, c.want)
		}
	}
}
