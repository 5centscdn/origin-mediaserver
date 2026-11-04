package icecast

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// Pull plays an Icecast/SHOUTcast (or any plain HTTP) audio stream, e.g.
// http://host:8000/stream.mp3, and publishes its frames on bus until ctx
// is done or the stream ends: the pull counterpart of the source push
// handled by Server. Same scope as push: MP3 and AAC (ADTS) only.
//
// The codec comes from Content-Type; when that's missing or generic
// (application/octet-stream, which some servers send), it's sniffed from
// the first bytes. Icy-MetaData isn't requested, so the body is pure audio
// with no in-band title updates to strip.
func Pull(ctx context.Context, url string, bus *media.Bus) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "origin-mediaserver")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}

	body := bufio.NewReaderSize(resp.Body, 16*1024)
	codec, ok := codecFromContentType(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if !ok {
		head, _ := body.Peek(4096)
		if codec, ok = sniffAudioCodec(head); !ok {
			return fmt.Errorf("get %s: not MP3 or AAC audio (Content-Type %q)", url, resp.Header.Get("Content-Type"))
		}
	}
	return newSource(bus, codec).run(ctx, body)
}

// sniffAudioCodec recognizes MP3 (an ID3v2 tag or MPEG audio frame sync
// followed by a second valid frame header where the first frame says it
// ends) or ADTS AAC at the start of data.
func sniffAudioCodec(data []byte) (media.CodecType, bool) {
	if len(data) >= 3 && string(data[:3]) == "ID3" {
		return media.CodecMP3, true
	}
	for i := 0; i+7 <= len(data); i++ {
		if data[i] != 0xFF || data[i+1]&0xE0 != 0xE0 {
			continue
		}
		if info, ok := parseADTSHeader(data[i:]); ok && data[i+1]&0x06 == 0 {
			if n := i + info.length; n+2 <= len(data) && data[n] == 0xFF && data[n+1]&0xF6 == 0xF0 {
				return media.CodecAAC, true
			}
		}
		if info, ok := parseMP3Header(data[i:]); ok {
			if n := i + info.length; n+2 <= len(data) && data[n] == 0xFF && data[n+1]&0xE0 == 0xE0 {
				return media.CodecMP3, true
			}
		}
	}
	return media.CodecUnknown, false
}
