package db

import (
	"context"
	"testing"
)

// An exact stream config wins the settings lookup, but the %i pattern still
// decides FMLE ABR grouping (GetABRPatternConfig): creating test1 on the
// Streams page must not pull it out of the automatic "test" group.
func TestStreamConfigPrecedenceAndABRPattern(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, `INSERT INTO applications (id, name) VALUES ('a1', 'vineeth')`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id, pat string
		wild    int
	}{{"s1", "test%i", 1}, {"s2", "test1", 0}, {"s3", "cam*", 1}} {
		if _, err := d.ExecContext(ctx, `INSERT INTO stream_configs (id, app_id, name_pattern, is_wildcard) VALUES (?, 'a1', ?, ?)`,
			c.id, c.pat, c.wild); err != nil {
			t.Fatal(err)
		}
	}

	sc, _ := d.GetStreamConfig(ctx, "a1", "test1")
	if sc == nil || sc.NamePattern != "test1" {
		t.Fatalf("settings for test1 = %+v, want the exact config", sc)
	}
	for name, want := range map[string]string{"test1": "test%i", "test7": "test%i", "cam2": "", "testx": ""} {
		pat, err := d.GetABRPatternConfig(ctx, "a1", name)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if pat != nil {
			got = pat.NamePattern
		}
		if got != want {
			t.Errorf("ABR pattern for %s = %q, want %q", name, got, want)
		}
	}
}

// PublishLimits returns every limited wildcard config a name falls under,
// including when an exact config wins the settings lookup.
func TestPublishLimits(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, `INSERT INTO applications (id, name) VALUES ('a1', 'live');
		INSERT INTO stream_configs (id, app_id, name_pattern, is_wildcard, max_streams, enabled) VALUES
		  ('s1', 'a1', 'test%i', 1, 3, 1), ('s2', 'a1', 'test1', 0, 0, 1),
		  ('s3', 'a1', 'cam*', 1, 0, 1), ('s4', 'a1', 'off%i', 1, 2, 0)`); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"test1": 3, "test12": 3, "cam1": 0, "off1": 0, "testx": 0} {
		lims, err := d.PublishLimits(ctx, "a1", name)
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		if len(lims) == 1 && lims[0].Match(name) {
			got = lims[0].Max
		}
		if len(lims) > 1 || got != want {
			t.Errorf("limits for %s = %+v, want max %d", name, lims, want)
		}
	}
}
