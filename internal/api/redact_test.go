package api

import "testing"

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"rtsp://admin:s3cr%40t@203.0.113.7:554/Streaming/Channels/301": "rtsp://admin:***@203.0.113.7:554/Streaming/Channels/301",
		"rtmp://user:pw@host/app/stream":  "rtmp://user:***@host/app/stream",
		"rtmp://host/app/stream":          "rtmp://host/app/stream",
		"rtsp://onlyuser@cam/1":           "rtsp://onlyuser@cam/1",
		"127.0.0.1:54692":                 "127.0.0.1:54692",
		"":                                "",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
