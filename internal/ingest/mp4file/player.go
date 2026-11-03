package mp4file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

// maxDownload caps an http(s) file fetched for playback.
const maxDownload = 4 << 30

// Play publishes the MP4 at src onto bus in real time, looping, until ctx
// is cancelled. src is a local path, a file:// URL, or an http(s) URL; a
// remote file is downloaded once into cacheDir and reused (so a failover
// doesn't wait on a download the second time). It sets the bus's CodecInfo
// before the first packet, as every packager initialises from it.
//
// Packets look like RTMP's: one per frame, H.264 as Annex B with a
// composition offset, AAC as raw frames. Timestamps start at 1 s and keep
// increasing across loops by the file's duration, so downstream outputs see
// one continuous stream.
func Play(ctx context.Context, src, cacheDir string, bus *media.Bus, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	path, err := localFile(ctx, src, cacheDir, log)
	if err != nil {
		return err
	}
	fh, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("mp4: %w", err)
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	f, err := parse(fh, st.Size())
	if err != nil {
		return err
	}
	bus.SetCodecInfo(f.codecInfo())

	evs, loopDur := f.timeline()
	log.Info("mp4 playback started", "src", src, "video", f.video != nil, "audio", f.audio != nil,
		"loop_s", float64(loopDur)/90000)

	const base = 90000 // first DTS: 1 s, so B-frame offsets never go negative
	start := time.Now()
	var buf []byte
	for loop := int64(0); ; loop++ {
		off := base + loop*loopDur
		for _, ev := range evs {
			dts := off + ev.dts
			// Pace on DTS against the wall clock.
			if wait := time.Until(start.Add(time.Duration(dts-base) * time.Second / 90000)); wait > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
			s := ev.t.samples[ev.i]
			if cap(buf) < int(s.size) {
				buf = make([]byte, s.size)
			}
			data := buf[:s.size]
			if _, err := fh.ReadAt(data, s.off); err != nil {
				return fmt.Errorf("mp4: read sample: %w", err)
			}
			pkt := &media.Packet{
				DTS:       dts,
				PTS:       off + ev.pts,
				WallClock: time.Now(),
				Type:      media.PacketMedia,
			}
			if ev.t.video {
				pkt.Codec = media.CodecH264
				pkt.Data = avccToAnnexB(data, ev.t.lengthSize)
				pkt.CompositionTimeOffset = pkt.PTS - pkt.DTS
				if s.key {
					pkt.IsKeyframe = true
					pkt.Type = media.PacketKeyframe
				}
			} else {
				pkt.Codec = media.CodecAAC
				pkt.Data = append([]byte(nil), data...)
			}
			bus.Publish(pkt)
		}
	}
}

// event is one sample on the merged playback timeline (90 kHz, relative to
// the start of a loop).
type event struct {
	t        *track
	i        int
	dts, pts int64
}

// timeline merges the tracks' samples in DTS order and returns the loop
// length: the longer track's span, so neither track overlaps itself when
// the next loop starts (the shorter one gets a small gap instead).
func (f *file) timeline() ([]event, int64) {
	var evs []event
	var loop int64
	for _, t := range []*track{f.video, f.audio} {
		if t == nil {
			continue
		}
		ts := int64(t.timescale)
		to90 := func(v int64) int64 { return v * 90000 / ts }
		n := len(t.samples)
		for i, s := range t.samples {
			evs = append(evs, event{t: t, i: i, dts: to90(s.dts - t.shift), pts: to90(s.dts + int64(s.cto) - t.shift)})
		}
		// Span: first DTS to last DTS plus the last sample's own duration.
		// Measured from the first sample, not 0: an AAC track usually
		// starts before 0 (encoder priming, shifted by its edit list), and
		// the next loop must start after this one's last sample.
		first, last := t.samples[0].dts, t.samples[n-1].dts
		step := int64(1)
		if n > 1 {
			step = last - t.samples[n-2].dts
		}
		if d := to90(last+step) - to90(first); d > loop {
			loop = d
		}
	}
	sort.SliceStable(evs, func(a, b int) bool { return evs[a].dts < evs[b].dts })
	if loop <= 0 {
		loop = 90000
	}
	return evs, loop
}

// codecInfo builds the CodecInfo outputs initialise from, matching what
// the RTMP path derives from its AVC/AAC sequence headers.
func (f *file) codecInfo() *media.CodecInfo {
	ci := &media.CodecInfo{}
	if v := f.video; v != nil {
		ci.VideoCodec = media.CodecH264
		ci.Profile, ci.ProfileCompatibility, ci.Level = v.avcC[1], v.avcC[2], v.avcC[3]
		ci.SPSNALu, ci.PPSNALu = v.sps, v.pps
		ci.Width, ci.Height = media.ParseSPSResolution(v.sps)
		if n := len(v.samples); n > 1 {
			if d := v.samples[n-1].dts - v.samples[0].dts; d > 0 {
				ci.FrameRate = float64(n-1) * float64(v.timescale) / float64(d)
			}
		}
	}
	if a := f.audio; a != nil {
		ci.AudioCodec = media.CodecAAC
		ci.AACConfig = a.asc
		ci.SampleRate = mux.ParseAACSampleRate(a.asc)
		if len(a.asc) >= 2 {
			ci.Channels = int(a.asc[1]>>3) & 0x0F
		}
	}
	return ci
}

// avccToAnnexB rewrites length-prefixed NAL units with start codes.
func avccToAnnexB(b []byte, lengthSize int) []byte {
	out := make([]byte, 0, len(b)+16)
	for len(b) >= lengthSize {
		n := 0
		for i := 0; i < lengthSize; i++ {
			n = n<<8 | int(b[i])
		}
		b = b[lengthSize:]
		if n > len(b) {
			break
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, b[:n]...)
		b = b[n:]
	}
	return out
}

// localFile resolves src to a readable local path, downloading http(s)
// sources into cacheDir once.
func localFile(ctx context.Context, src, cacheDir string, log *slog.Logger) (string, error) {
	switch {
	case strings.HasPrefix(src, "http://"), strings.HasPrefix(src, "https://"):
	case strings.HasPrefix(src, "file://"):
		return strings.TrimPrefix(src, "file://"), nil
	default:
		if strings.Contains(src, "://") {
			return "", fmt.Errorf("mp4: unsupported URL scheme in %q (use http(s)://, file:// or a path)", src)
		}
		return src, nil
	}
	sum := sha256.Sum256([]byte(src))
	path := filepath.Join(cacheDir, hex.EncodeToString(sum[:12])+".mp4")
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		return path, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	log.Info("downloading mp4 for playback", "url", src)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("mp4: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mp4: download %s: HTTP %d", src, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(cacheDir, "dl-*.part")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxDownload+1))
	tmp.Close()
	if err == nil && n > maxDownload {
		err = errors.New("mp4: file larger than 4 GB")
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("mp4: download: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}
