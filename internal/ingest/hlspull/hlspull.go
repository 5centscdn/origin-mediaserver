// Package hlspull pulls a live HLS stream (an http(s) .m3u8 URL) and
// publishes it onto a media.Bus, for internal/pull.Manager's "hls"
// protocol.
//
// The segments are concatenated into one continuous MPEG-TS byte stream and
// read by a single mpegts.Demuxer, exactly as an HTTP MPEG-TS pull is, so
// continuity counters, PSI and the H.264 retimer carry across segment
// boundaries.
//
// Scope:
//   - MPEG-TS segments. fMP4/CMAF segments (EXT-X-MAP) and encrypted
//     segments (EXT-X-KEY other than NONE) are refused with an error, as are
//     byte-range segments (EXT-X-BYTERANGE).
//   - A master playlist is followed to its highest-BANDWIDTH variant; an
//     origin wants the best rendition to repackage. Alternate audio
//     renditions (EXT-X-MEDIA) aren't fetched, so their audio is left out.
//   - Starts liveStartSegments from the live edge, then fetches each new
//     segment once, in media sequence order. If the source's window moves
//     past the next wanted segment (we fell behind), it skips to the oldest
//     one listed; a sequence that goes backwards (encoder restart) restarts
//     from the live edge.
//   - The playlist is reloaded every half target duration (RFC 8216
//     6.3.4's rate when it hasn't changed). If it doesn't advance for
//     stallTargets target durations, Pull returns an error and the pull
//     manager reconnects. EXT-X-ENDLIST ends the pull once its segments
//     are fed.
//   - Variant and segment URIs are resolved against the playlist URL and
//     must be http or https.
package hlspull

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/ingest/mpegts"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

const (
	liveStartSegments = 2
	stallTargets      = 4
	maxPlaylistBytes  = 4 << 20
	maxSegmentBytes   = 256 << 20
	defaultTarget     = 6 * time.Second
)

var client = &http.Client{}

// segment is one media playlist entry.
type segment struct {
	seq uint64
	uri string
}

// playlist is what Pull needs from a fetched .m3u8.
type playlist struct {
	variants []variant // master playlist only
	target   time.Duration
	segments []segment
	ended    bool
}

type variant struct {
	bandwidth int
	uri       string
}

// Pull follows the HLS stream at rawURL until ctx is done, the stream ends
// or stalls, or a fetch fails. It blocks, like mpegts.PullHTTP.
func Pull(ctx context.Context, rawURL string, bus *media.Bus) error {
	base, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("hls: bad url: %w", err)
	}
	pl, base, err := fetchPlaylist(ctx, base, 15*time.Second)
	if err != nil {
		return err
	}
	if len(pl.variants) > 0 {
		best := pl.variants[0]
		for _, v := range pl.variants[1:] {
			if v.bandwidth > best.bandwidth {
				best = v
			}
		}
		vu, err := resolve(base, best.uri)
		if err != nil {
			return err
		}
		if pl, base, err = fetchPlaylist(ctx, vu, 15*time.Second); err != nil {
			return err
		}
		if len(pl.variants) > 0 {
			return errors.New("hls: variant playlist is another master playlist")
		}
	}

	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	feedErr := make(chan error, 1)
	go func() {
		err := feed(ctx, base, pl, pw)
		pw.CloseWithError(err) // nil → EOF for the demuxer
		feedErr <- err
	}()

	d := mpegts.NewDemuxer(bus)
	d.SetLogContext("source", "hls")
	d.OnBytes = func(n int) { bus.AddBytesIn(int64(n)) }
	runErr := d.Run(ctx, pr)
	cancel()
	pr.CloseWithError(context.Canceled) // unblock a feeder mid-write
	if err := <-feedErr; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return runErr
}

// feed writes the stream's segments to w in order, reloading the media
// playlist at base, starting from pl.
func feed(ctx context.Context, base *url.URL, pl *playlist, w io.Writer) error {
	var next uint64
	started := false
	lastProgress := time.Now()
	for {
		if n := len(pl.segments); n > 0 {
			first, last := pl.segments[0].seq, pl.segments[n-1].seq
			switch {
			case !started:
				next = first
				if n > liveStartSegments && !pl.ended {
					next = pl.segments[n-liveStartSegments].seq
				}
				started = true
			case next < first:
				next = first // fell behind the source's window
			case next > last+1:
				// Sequence went backwards: the source restarted.
				next = pl.segments[max(n-liveStartSegments, 0)].seq
			}
			for _, s := range pl.segments {
				if s.seq < next {
					continue
				}
				su, err := resolve(base, s.uri)
				if err != nil {
					return err
				}
				if err := fetchSegment(ctx, su, w, 2*pl.target+10*time.Second); err != nil {
					return err
				}
				next = s.seq + 1
				lastProgress = time.Now()
			}
		}
		if pl.ended {
			return nil
		}
		if time.Since(lastProgress) > stallTargets*pl.target {
			return fmt.Errorf("hls: playlist hasn't advanced for %s", time.Since(lastProgress).Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pl.target / 2):
		}
		np, _, err := fetchPlaylist(ctx, base, pl.target+10*time.Second)
		if err != nil {
			return err
		}
		if len(np.variants) > 0 {
			return errors.New("hls: media playlist turned into a master playlist")
		}
		pl = np
	}
}

// fetchPlaylist GETs and parses u. The returned URL is the one to resolve
// relative URIs against: after redirects, the final location.
func fetchPlaylist(ctx context.Context, u *url.URL, timeout time.Duration) (*playlist, *url.URL, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := get(ctx, u)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	pl, err := parse(io.LimitReader(resp.Body, maxPlaylistBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("hls: %s: %w", redact(u), err)
	}
	return pl, resp.Request.URL, nil
}

// fetchSegment copies one segment's body to w.
func fetchSegment(ctx context.Context, u *url.URL, w io.Writer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := get(ctx, u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, io.LimitReader(resp.Body, maxSegmentBytes)); err != nil {
		if ctx.Err() != nil && !errors.Is(err, io.ErrClosedPipe) {
			return fmt.Errorf("hls: segment %s: %w", redact(u), ctx.Err())
		}
		return err
	}
	return nil
}

func get(ctx context.Context, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error's message carries the full URL (tokens included).
		if inner := errors.Unwrap(err); inner != nil {
			err = inner
		}
		return nil, fmt.Errorf("hls: get %s: %w", redact(u), err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("hls: get %s: status %d", redact(u), resp.StatusCode)
	}
	return resp, nil
}

// resolve resolves ref against base, allowing only http(s) results.
func resolve(base *url.URL, ref string) (*url.URL, error) {
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil, fmt.Errorf("hls: bad uri %q: %w", ref, err)
	}
	u := base.ResolveReference(r)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("hls: uri %q is not http(s)", ref)
	}
	return u, nil
}

// redact drops credentials and the query (often a session token) from u
// for error messages.
func redact(u *url.URL) string {
	c := *u
	c.User, c.RawQuery = nil, ""
	return c.String()
}

// parse reads an M3U8 playlist: master (variants) or media (segments).
func parse(r io.Reader) (*playlist, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	pl := &playlist{target: defaultTarget}
	var (
		seq        uint64
		inSegment  bool // saw EXTINF, waiting for its URI
		pendingBW  = -1 // saw EXT-X-STREAM-INF, waiting for its URI
		first      = true
		sawSegment bool
	)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			if !strings.HasPrefix(line, "#EXTM3U") {
				return nil, errors.New("not an M3U8 playlist")
			}
			first = false
			continue
		}
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			pendingBW = 0
			if v, ok := attr(line, "BANDWIDTH"); ok {
				pendingBW, _ = strconv.Atoi(v)
			}
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			if n, err := strconv.ParseFloat(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"), 64); err == nil && n > 0 {
				pl.target = time.Duration(n * float64(time.Second))
			}
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			if n, err := strconv.ParseUint(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64); err == nil && !sawSegment {
				seq = n
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			inSegment = true
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			pl.ended = true
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			return nil, errors.New("fMP4/CMAF segments (EXT-X-MAP) aren't supported, only MPEG-TS")
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			return nil, errors.New("byte-range segments aren't supported")
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			if m, _ := attr(line, "METHOD"); m != "NONE" {
				return nil, fmt.Errorf("encrypted segments (METHOD=%s) aren't supported", m)
			}
		case strings.HasPrefix(line, "#"):
			// Other tags (and comments) don't change what's fetched.
		case pendingBW >= 0:
			pl.variants = append(pl.variants, variant{bandwidth: pendingBW, uri: line})
			pendingBW = -1
		case inSegment:
			pl.segments = append(pl.segments, segment{seq: seq, uri: line})
			seq++
			inSegment, sawSegment = false, true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if first {
		return nil, errors.New("empty playlist")
	}
	return pl, nil
}

// attr returns attribute key's value from an M3U8 attribute list line.
func attr(line, key string) (string, bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", false
	}
	rest := line[i+1:]
	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			return "", false
		}
		k := strings.TrimSpace(rest[:eq])
		rest = rest[eq+1:]
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				return "", false
			}
			v, rest = rest[1:end+1], rest[end+2:]
		} else if c := strings.IndexByte(rest, ','); c >= 0 {
			v, rest = rest[:c], rest[c:]
		} else {
			v, rest = rest, ""
		}
		rest = strings.TrimPrefix(rest, ",")
		if k == key {
			return v, true
		}
	}
	return "", false
}
