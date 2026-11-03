package db

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// The database has one connection (SetMaxOpenConns(1)), and requests and
// stream lifecycles cancel their contexts mid-query all the time (a pull
// source reconnecting, a client going away). modernc.org/sqlite v1.29.6
// could leave that connection with SQLite's interrupt flag stuck, after
// which every query failed with "interrupted (9)" until a restart: the
// whole API and panel went down. Reproduced with this test (failing about
// 2 runs in 3) and fixed by upgrading the driver.
func TestCancelledQueriesLeaveConnUsable(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				c, cancel := context.WithTimeout(ctx, time.Duration(rand.Intn(200))*time.Microsecond)
				var n int
				_ = d.QueryRowContext(c, `SELECT count(*) FROM applications`).Scan(&n)
				_, _ = d.GetApplicationByName(c, "nope")
				cancel()
			}
		}()
	}
	wg.Wait()
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM applications`).Scan(&n); err != nil {
		t.Fatalf("after cancelled queries: %v", err)
	}
}
