package mpegts

import (
	"context"
	"fmt"
	"net/http"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// PullHTTP GETs url and demuxes the (chunked/continuous) MPEG-TS response
// body onto bus until ctx is done, the connection ends, or a fatal error
// occurs. Mirrors rtmp.Pull's blocking contract for
// internal/pull.Manager's supervisor loop.
func PullHTTP(ctx context.Context, url string, bus *media.Bus) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}

	d := NewDemuxer(bus)
	d.OnBytes = func(n int) { bus.AddBytesIn(int64(n)) }
	return d.Run(ctx, resp.Body)
}
