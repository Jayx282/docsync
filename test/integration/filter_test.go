//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/Jayx282/docsync/internal/config"
)

// TestRootFilter indexes only unsold stones. Random status changes move stones in and
// out of the index, so incremental output must keep equalling a full rebuild with the
// same filter, and must hold exactly the unsold stones. Changing the filter is a
// config change and starts a new epoch.
func TestRootFilter(t *testing.T) {
	ctx := context.Background()
	s := seed(t)
	t.Logf("seed %d (replay with DOCSYNC_TEST_SEED=%d)", s, s)
	rng := rand.New(rand.NewSource(s))
	fx := newFixture(t, s, 150)

	unsold := func(c *config.Config) { c.Document.Root.Filter = "t.status <> 'sold'" }
	dir := t.TempDir()
	e := openEngine(t, testConfig(t, fx.dsn, dir, 37, unsold), nil)

	expect := func(inc map[string]string, where string, keep func(status string) bool, label string) {
		t.Helper()
		var want int
		if err := fx.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dbo.stone WHERE "+where).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if len(inc) != want {
			t.Fatalf("%s: %d documents, but %d stones match the filter", label, len(inc), want)
		}
		for k, js := range inc {
			var d map[string]any
			if err := json.Unmarshal([]byte(js), &d); err != nil {
				t.Fatal(err)
			}
			if st, _ := d["status"].(string); !keep(st) {
				t.Fatalf("%s: document %s has status %q, which the filter excludes", label, k, st)
			}
		}
	}

	n := rounds(120)
	for round := 1; round <= n; round++ {
		if _, err := fx.gen.RandomTransaction(ctx, 6); err != nil {
			t.Fatalf("round %d writer: %v", round, err)
		}
		for steps := rng.Intn(3); steps > 0; steps-- {
			if _, err := e.Step(ctx); err != nil {
				t.Fatalf("round %d step: %v", round, err)
			}
		}
		if round%40 == 0 || round == n {
			label := fmt.Sprintf("round %d", round)
			inc := check(t, ctx, fx.dsn, dir, e, label, unsold)
			expect(inc.Docs, "status <> 'sold'", func(st string) bool { return st != "sold" }, label)
		}
	}
	e.Close()

	// A different filter is a different document definition: new epoch, new contents.
	stockOnly := func(c *config.Config) { c.Document.Root.Filter = "status IN ('stock', 'memo')" }
	e = openEngine(t, testConfig(t, fx.dsn, dir, 37, stockOnly), nil)
	defer e.Close()
	inc := check(t, ctx, fx.dsn, dir, e, "new filter", stockOnly)
	if inc.Epoch != 2 {
		t.Fatalf("changing the filter should start epoch 2, output is at epoch %d", inc.Epoch)
	}
	expect(inc.Docs, "status IN ('stock', 'memo')", func(st string) bool { return st == "stock" || st == "memo" }, "new filter")
}
