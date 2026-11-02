package pull

import (
	"context"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/db"
)

// TestSupportedProtocols_IncludesSRT guards the SRT Pull/Caller wiring
// (internal/srt.Pull): "srt" must be a recognized protocol value, the same
// way "rtmp"/"rtsp"/"mpegts_http"/"mpegts_udp" already are — a one-line map
// entry is all SRT pull needed, since internal/srt.Pull reuses the exact
// same mpegts.Demuxer as every other protocol here.
func TestSupportedProtocols_IncludesSRT(t *testing.T) {
	if _, ok := supportedProtocols["srt"]; !ok {
		t.Fatal(`supportedProtocols["srt"] not registered — SRT Pull/Caller ingest is unreachable via the pull manager`)
	}
}

// TestManager_Start_UnsupportedProtocolErrors confirms the negative path
// still works: an unrecognized protocol value is rejected immediately
// (before touching the database or spawning a worker), which is what makes
// the positive "srt" case above meaningful rather than a fluke of Start
// accepting everything.
func TestManager_Start_UnsupportedProtocolErrors(t *testing.T) {
	m := NewManager(nil, nil, nil)
	err := m.Start(context.Background(), db.PullSource{ID: "x", Protocol: "carrier-pigeon"})
	if err == nil {
		t.Fatal("Start with an unsupported protocol returned nil error, want an error")
	}
}
