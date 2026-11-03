package sysmon

import (
	"testing"
	"time"
)

// BenchmarkSample measures one full collection pass against the real /proc
// (CPU, memory, disks, NICs, socket tables). It runs every 5 s in production.
func BenchmarkSample(b *testing.B) {
	c := New(Config{DataDir: "/", DVRPath: "/", HTTPListen: ":8080", RTMPListen: ":1935", SRTListen: ":4000"}, Sources{})
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		now = now.Add(5 * time.Second)
		c.sample(now)
	}
}
