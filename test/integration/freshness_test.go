//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/sink"
)

// TestFreshness measures the time from a committed source change to the moment it is
// visible to a consumer of the sink, at 1,000 changed stones per minute, with the
// engine running its real loop (poll every 500 ms, a segment at most every 500 ms).
func TestFreshness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx := newFixture(t, seed(t), 2000)
	dir := t.TempDir()
	cfg := testConfig(t, fx.dsn, dir, 5000, func(c *config.Config) {
		c.Source.PollInterval.Duration = 500 * time.Millisecond
		c.Sinks = c.Sinks[:1] // ndjson only: easy to tail
		c.Sinks[0].SegmentInterval.Duration = 500 * time.Millisecond
	})
	e := openEngine(t, cfg, nil)
	defer e.Close()
	catchUp(t, ctx, e)

	runDone := make(chan error, 1)
	go func() { runDone <- e.Run(ctx) }()

	ids := make([]int64, 0, 2000)
	rows, err := fx.db.Query("SELECT stoneid FROM dbo.stone ORDER BY stoneid")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()

	var mu sync.Mutex
	committed := map[string]time.Time{}
	seen := map[string]time.Time{}

	// Consumer: poll the manifest and read each new segment once.
	outDir := filepath.Join(dir, "out", "ndjson")
	stopTail := make(chan struct{})
	tailDone := make(chan struct{})
	go func() {
		defer close(tailDone)
		read := map[string]bool{}
		for {
			select {
			case <-stopTail:
				return
			case <-time.After(100 * time.Millisecond):
			}
			b, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
			if err != nil {
				continue
			}
			var m sink.Manifest
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			for _, seg := range m.Segments {
				if read[seg.Path] {
					continue
				}
				read[seg.Path] = true
				f, err := os.Open(filepath.Join(outDir, filepath.FromSlash(seg.Path)))
				if err != nil {
					continue
				}
				now := time.Now()
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 1<<20), 16<<20)
				for sc.Scan() {
					var l struct {
						Doc map[string]any `json:"doc"`
					}
					if json.Unmarshal(sc.Bytes(), &l) != nil || l.Doc == nil {
						continue
					}
					if s, ok := l.Doc["status"].(string); ok && strings.HasPrefix(s, "m") {
						mu.Lock()
						if _, dup := seen[s]; !dup {
							seen[s] = now
						}
						mu.Unlock()
					}
				}
				f.Close()
			}
		}
	}()

	// Writer: 1,000 single-stone updates per minute for the test duration.
	duration := 30 * time.Second
	if v := os.Getenv("DOCSYNC_FRESHNESS_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			duration = time.Duration(n) * time.Second
		}
	}
	interval := time.Minute / 1000
	tick := time.NewTicker(interval)
	end := time.Now().Add(duration)
	seq := 0
	for time.Now().Before(end) {
		<-tick.C
		seq++
		marker := fmt.Sprintf("m%d", seq)
		id := ids[seq%len(ids)]
		if _, err := fx.db.Exec("UPDATE dbo.stone SET status = @p1 WHERE stoneid = @p2", marker, id); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		committed[marker] = time.Now()
		mu.Unlock()
	}
	tick.Stop()

	// Give the pipeline time to drain, then stop.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= len(committed)-countOverwritten(committed, len(ids)) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(stopTail)
	<-tailDone
	cancel()
	<-runDone

	var lat []time.Duration
	mu.Lock()
	for m, c := range committed {
		if s, ok := seen[m]; ok {
			lat = append(lat, s.Sub(c))
		}
	}
	mu.Unlock()
	if len(lat) == 0 {
		t.Fatal("no changes observed")
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration { return lat[int(p*float64(len(lat)-1))] }
	t.Logf("%d updates in %s, %d observed: p50 %s, p95 %s, p99 %s, max %s",
		len(committed), duration, len(lat), pct(.50).Round(time.Millisecond), pct(.95).Round(time.Millisecond),
		pct(.99).Round(time.Millisecond), lat[len(lat)-1].Round(time.Millisecond))
	if pct(.95) > 5*time.Second {
		t.Fatalf("p95 freshness %s exceeds the 5 s target", pct(.95))
	}
}

// countOverwritten estimates markers that a later update to the same stone replaced
// before it was published (only possible when updates outnumber stones).
func countOverwritten(committed map[string]time.Time, stones int) int {
	if len(committed) <= stones {
		return 0
	}
	return len(committed) - stones
}
