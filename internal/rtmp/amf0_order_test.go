package rtmp

import (
	"bytes"
	"testing"
)

// TestAMF0ObjectKeyOrder checks status objects always encode as
// level, code, description, as FMS does. FMLE 3.x crashes on any other order
// (see amf0FieldRank). Encoding many times guards against map randomisation.
func TestAMF0ObjectKeyOrder(t *testing.T) {
	obj := AMF0Object{
		"description": AMF0String("d"),
		"zzz":         AMF0String("z"),
		"code":        AMF0String("c"),
		"aaa":         AMF0String("a"),
		"level":       AMF0String("error"),
	}
	first, err := EncodeAMF0(obj)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"level", "code", "description", "aaa", "zzz"}
	last := -1
	for _, k := range want {
		i := bytes.Index(first, []byte(k))
		if i <= last {
			t.Fatalf("key %q out of order in %x", k, first)
		}
		last = i
	}
	for n := 0; n < 200; n++ {
		b, _ := EncodeAMF0(obj)
		if !bytes.Equal(b, first) {
			t.Fatalf("encoding not deterministic on iteration %d", n)
		}
	}
}
