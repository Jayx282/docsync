//go:build integration

// Package integration runs docsync against a real SQL Server.
//
//	DOCSYNC_TEST_DSN='sqlserver://sa:...@127.0.0.1:1433?database=master&encrypt=disable' \
//	  go test -tags integration ./test/integration/ -v
//
// DOCSYNC_TEST_SEED replays a failing run, DOCSYNC_TEST_ROUNDS changes its length and
// DOCSYNC_TEST_FAULTS=1 injects crashes at random points.
package integration

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"
)

// TestInvariant alternates random multi-table transactions with sync steps and,
// every 40 rounds, checks that the consumer view equals a fresh full rebuild.
func TestInvariant(t *testing.T) {
	ctx := context.Background()
	s := seed(t)
	t.Logf("seed %d (replay with DOCSYNC_TEST_SEED=%d)", s, s)
	rng := rand.New(rand.NewSource(s))
	fx := newFixture(t, s, 150)

	dir := t.TempDir()
	faultRate := 0.0
	if os.Getenv("DOCSYNC_TEST_FAULTS") != "" {
		faultRate = 0.08
	}
	faults := func(point string) error {
		if faultRate > 0 && rng.Float64() < faultRate {
			return fmt.Errorf("%w at %s", errInjected, point)
		}
		return nil
	}
	cfg := testConfig(t, fx.dsn, dir, 37, nil)
	e := openEngine(t, cfg, faults)
	defer func() { e.Close() }()

	n := rounds(120)
	crashes := 0
	for round := 1; round <= n; round++ {
		if _, err := fx.gen.RandomTransaction(ctx, 6); err != nil {
			t.Fatalf("round %d writer: %v", round, err)
		}
		for steps := rng.Intn(3); steps > 0; steps-- {
			if _, err := e.Step(ctx); err != nil {
				if !errors.Is(err, errInjected) {
					t.Fatalf("round %d step: %v", round, err)
				}
				crashes++
				e.Close()
				e = openEngine(t, cfg, faults)
			}
		}
		if round%40 == 0 || round == n {
			saved := faultRate
			faultRate = 0
			check(t, ctx, fx.dsn, dir, e, fmt.Sprintf("round %d", round), nil)
			faultRate = saved
		}
	}
	t.Logf("%d rounds, %d injected crashes", n, crashes)
}

// TestInvariantConcurrent runs writers at the same time as the syncer, so changes
// commit while cycles are reading, then checks convergence at pause points.
func TestInvariantConcurrent(t *testing.T) {
	ctx := context.Background()
	s := seed(t)
	t.Logf("seed %d (replay with DOCSYNC_TEST_SEED=%d)", s, s)
	fx := newFixture(t, s, 150)
	dir := t.TempDir()
	cfg := testConfig(t, fx.dsn, dir, 37, nil)
	e := openEngine(t, cfg, nil)
	defer e.Close()

	phases := rounds(120) / 40
	if phases < 1 {
		phases = 1
	}
	for phase := 1; phase <= phases; phase++ {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var writeErr error
		writes := 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := fx.gen.RandomTransaction(ctx, 4); err != nil {
					writeErr = err
					return
				}
				writes++
				time.Sleep(time.Duration(writes%5) * time.Millisecond)
			}
		}()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := e.Step(ctx); err != nil {
				close(stop)
				wg.Wait()
				t.Fatalf("phase %d step: %v", phase, err)
			}
		}
		close(stop)
		wg.Wait()
		if writeErr != nil {
			t.Fatalf("phase %d writer: %v", phase, writeErr)
		}
		check(t, ctx, fx.dsn, dir, e, fmt.Sprintf("phase %d (%d concurrent transactions)", phase, writes), nil)
	}
}
