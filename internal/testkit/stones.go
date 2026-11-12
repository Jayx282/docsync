package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// Stones drives random, seeded writes against examples/stones/schema.sql. It keeps
// its own model of which rows exist so operations stay deterministic for a seed.
type Stones struct {
	db     *sql.DB
	rng    *rand.Rand
	nextID int64

	stones     map[int64]bool    // existing stone ids
	deleted    map[int64]bool    // stone ids that were deleted (can be re-inserted)
	inclusions map[int64]int64   // inclusion_id -> stoneid
	rates      map[int64]int64   // rate_id -> stoneid
	labs       map[int64]string  // lab_id -> code
	orders     map[int64]bool    // order ids
	links      map[[2]int64]bool // (order_id, stoneid)
	lines      map[int64]int64   // line_id -> order_id
}

// NewStones creates a generator.
func NewStones(db *sql.DB, seed int64) *Stones {
	return &Stones{
		db: db, rng: rand.New(rand.NewSource(seed)), nextID: 1_000_000,
		stones: map[int64]bool{}, deleted: map[int64]bool{}, inclusions: map[int64]int64{},
		rates: map[int64]int64{}, labs: map[int64]string{}, orders: map[int64]bool{},
		links: map[[2]int64]bool{}, lines: map[int64]int64{},
	}
}

type stmt struct {
	sql  string
	args []any
}

type op struct {
	name  string
	stmts []stmt
	apply func()
}

func (g *Stones) id() int64 { g.nextID++; return g.nextID }

func keys64[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (g *Stones) pick(m []int64) (int64, bool) {
	if len(m) == 0 {
		return 0, false
	}
	return m[g.rng.Intn(len(m))], true
}

func (g *Stones) pickStone() (int64, bool) { return g.pick(keys64(g.stones)) }
func (g *Stones) pickOrder() (int64, bool) { return g.pick(keys64(g.orders)) }
func (g *Stones) pickLabID() (int64, bool) { return g.pick(keys64(g.labs)) }
func (g *Stones) pickAnyStone() (int64, bool) {
	ids := append(keys64(g.stones), keys64(g.deleted)...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return g.pick(ids)
}

func (g *Stones) labCode() any {
	switch r := g.rng.Intn(10); {
	case r == 0:
		return nil
	case r == 1:
		return fmt.Sprintf("ZZ%d", g.rng.Intn(5)) // dangling code that a later lab insert may satisfy
	default:
		if id, ok := g.pickLabID(); ok {
			return g.labs[id]
		}
		return nil
	}
}

var (
	shapes   = []string{"round", "oval", "pear", "princess", "cushion"}
	colors   = []string{"D", "E", "F", "G", "H", "I", "J"}
	clarity  = []string{"IF", "VVS1", "VVS2", "VS1", "VS2", "SI1"}
	statuses = []string{"stock", "memo", "sold", "hold"}
	kinds    = []string{"crystal", "feather", "cloud", "needle", "pinpoint"}
)

func (g *Stones) one(xs []string) string { return xs[g.rng.Intn(len(xs))] }

func (g *Stones) stoneValues(id int64) []any {
	var cert any
	if g.rng.Intn(3) > 0 {
		cert = time.Date(2020+g.rng.Intn(6), time.Month(1+g.rng.Intn(12)), 1+g.rng.Intn(28), 0, 0, 0, 0, time.UTC)
	}
	return []any{id, fmt.Sprintf("P-%d", id), fmt.Sprintf("%d.%03d", g.rng.Intn(5), g.rng.Intn(1000)),
		g.one(shapes), g.one(colors), g.one(clarity), g.labCode(), g.one(statuses), cert,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(g.rng.Intn(1_000_000)) * time.Second)}
}

const insertStone = `INSERT INTO dbo.stone (stoneid, packet_no, carat, shape, color, clarity, lab_code, status, certified_on, updated_at)
VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10)`

// Seed loads an initial data set.
func (g *Stones) Seed(ctx context.Context, nStones, nLabs, nOrders int) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	}
	for i := 1; i <= nLabs; i++ {
		code := fmt.Sprintf("L%02d", i)
		if err := exec(`INSERT INTO dbo.lab (lab_id, code, name, country) VALUES (@p1, @p2, @p3, @p4)`,
			int64(i), code, fmt.Sprintf("Lab %d", i), g.one([]string{"BE", "IN", "US", "IL"})); err != nil {
			return err
		}
		g.labs[int64(i)] = code
	}
	for i := 0; i < nStones; i++ {
		id := g.id()
		if err := exec(insertStone, g.stoneValues(id)...); err != nil {
			return err
		}
		g.stones[id] = true
		for j := g.rng.Intn(4); j > 0; j-- {
			iid := g.id()
			if err := exec(`INSERT INTO dbo.inclusion (inclusion_id, stoneid, kind, position, severity) VALUES (@p1, @p2, @p3, @p4, @p5)`,
				iid, id, g.one(kinds), int64(g.rng.Intn(10)), int64(g.rng.Intn(6))); err != nil {
				return err
			}
			g.inclusions[iid] = id
		}
		for j := 1 + g.rng.Intn(2); j > 0; j-- {
			rid := g.id()
			if err := exec(`INSERT INTO dbo.rate (rate_id, stoneid, rate_type, amount, currency) VALUES (@p1, @p2, @p3, @p4, @p5)`,
				rid, id, g.one([]string{"list", "cost", "offer"}), fmt.Sprintf("%d.%02d", 100+g.rng.Intn(9000), g.rng.Intn(100)), "USD"); err != nil {
				return err
			}
			g.rates[rid] = id
		}
	}
	for i := 0; i < nOrders; i++ {
		oid := g.id()
		if err := exec(`INSERT INTO dbo.order_header (order_id, party, order_date, status) VALUES (@p1, @p2, @p3, @p4)`,
			oid, fmt.Sprintf("Party %d", g.rng.Intn(50)), time.Date(2026, time.Month(1+g.rng.Intn(10)), 1+g.rng.Intn(28), 0, 0, 0, 0, time.UTC), "open"); err != nil {
			return err
		}
		g.orders[oid] = true
		for j := 1 + g.rng.Intn(3); j > 0; j-- {
			sid, _ := g.pickStone()
			k := [2]int64{oid, sid}
			if g.links[k] {
				continue
			}
			if err := exec(`INSERT INTO dbo.order_stone (order_id, stoneid) VALUES (@p1, @p2)`, oid, sid); err != nil {
				return err
			}
			g.links[k] = true
		}
		for j := g.rng.Intn(3); j > 0; j-- {
			lid := g.id()
			if err := exec(`INSERT INTO dbo.order_line (line_id, order_id, note, amount) VALUES (@p1, @p2, @p3, @p4)`,
				lid, oid, "seed", fmt.Sprintf("%d.00", 10+g.rng.Intn(500))); err != nil {
				return err
			}
			g.lines[lid] = oid
		}
	}
	return tx.Commit()
}

// RandomTransaction applies 1..maxOps random operations in one transaction and
// returns their names. A transaction that fails (for example on a duplicate key)
// is rolled back and reported with an empty list.
func (g *Stones) RandomTransaction(ctx context.Context, maxOps int) ([]string, error) {
	n := 1 + g.rng.Intn(maxOps)
	var ops []op
	for len(ops) < n {
		if o := g.randomOp(); o != nil {
			ops = append(ops, *o)
			o.apply() // model the effect immediately so later ops in this tx see it
		}
	}
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range ops {
		for _, s := range o.stmts {
			if _, err := tx.ExecContext(ctx, s.sql, s.args...); err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("%s: %w", o.name, err)
			}
		}
		names = append(names, o.name)
	}
	return names, tx.Commit()
}

func (g *Stones) randomOp() *op {
	type choice struct {
		w int
		f func() *op
	}
	choices := []choice{
		{8, g.opInsertStone}, {10, g.opUpdateStone}, {4, g.opNoopStone}, {3, g.opDeleteStone}, {2, g.opReinsertStone},
		{6, g.opInsertInclusion}, {5, g.opUpdateInclusion}, {4, g.opMoveInclusion}, {4, g.opDeleteInclusion}, {2, g.opRekeyInclusion},
		{4, g.opInsertRate}, {4, g.opUpdateRate}, {2, g.opMoveRate}, {2, g.opDeleteRate},
		{3, g.opRenameLab}, {2, g.opRecodeLab}, {1, g.opDeleteLab}, {1, g.opInsertLab},
		{3, g.opInsertOrder}, {3, g.opUpdateOrder}, {1, g.opDeleteOrder},
		{4, g.opLink}, {3, g.opUnlink}, {2, g.opMoveLink},
		{4, g.opInsertLine}, {3, g.opUpdateLine}, {2, g.opMoveLine}, {2, g.opDeleteLine},
		{1, g.opBurst},
	}
	total := 0
	for _, c := range choices {
		total += c.w
	}
	r := g.rng.Intn(total)
	for _, c := range choices {
		if r < c.w {
			return c.f()
		}
		r -= c.w
	}
	return nil
}

func (g *Stones) opInsertStone() *op {
	id := g.id()
	return &op{"insert stone", []stmt{{insertStone, g.stoneValues(id)}}, func() { g.stones[id] = true }}
}

func (g *Stones) opUpdateStone() *op {
	id, ok := g.pickStone()
	if !ok {
		return nil
	}
	switch g.rng.Intn(4) {
	case 0:
		return &op{"update stone carat", []stmt{{`UPDATE dbo.stone SET carat = @p1, updated_at = SYSUTCDATETIME() WHERE stoneid = @p2`, []any{fmt.Sprintf("%d.%03d", g.rng.Intn(5), g.rng.Intn(1000)), id}}}, func() {}}
	case 1:
		return &op{"update stone color", []stmt{{`UPDATE dbo.stone SET color = @p1 WHERE stoneid = @p2`, []any{g.one(colors), id}}}, func() {}}
	case 2:
		return &op{"update stone status", []stmt{{`UPDATE dbo.stone SET status = @p1 WHERE stoneid = @p2`, []any{g.one(statuses), id}}}, func() {}}
	default:
		return &op{"update stone lab", []stmt{{`UPDATE dbo.stone SET lab_code = @p1 WHERE stoneid = @p2`, []any{g.labCode(), id}}}, func() {}}
	}
}

func (g *Stones) opNoopStone() *op {
	id, ok := g.pickStone()
	if !ok {
		return nil
	}
	return &op{"no-op update stone", []stmt{{`UPDATE dbo.stone SET color = color WHERE stoneid = @p1`, []any{id}}}, func() {}}
}

func (g *Stones) opDeleteStone() *op {
	id, ok := g.pickStone()
	if !ok {
		return nil
	}
	return &op{"delete stone", []stmt{{`DELETE FROM dbo.stone WHERE stoneid = @p1`, []any{id}}}, func() {
		delete(g.stones, id)
		g.deleted[id] = true
	}}
}

func (g *Stones) opReinsertStone() *op {
	id, ok := g.pick(keys64(g.deleted))
	if !ok {
		return nil
	}
	return &op{"re-insert stone", []stmt{{insertStone, g.stoneValues(id)}}, func() {
		delete(g.deleted, id)
		g.stones[id] = true
	}}
}

func (g *Stones) opInsertInclusion() *op {
	sid, ok := g.pickAnyStone()
	if !ok {
		return nil
	}
	iid := g.id()
	return &op{"insert inclusion", []stmt{{`INSERT INTO dbo.inclusion (inclusion_id, stoneid, kind, position, severity) VALUES (@p1, @p2, @p3, @p4, @p5)`,
		[]any{iid, sid, g.one(kinds), int64(g.rng.Intn(10)), int64(g.rng.Intn(6))}}}, func() { g.inclusions[iid] = sid }}
}

func (g *Stones) opUpdateInclusion() *op {
	iid, ok := g.pick(keys64(g.inclusions))
	if !ok {
		return nil
	}
	return &op{"update inclusion", []stmt{{`UPDATE dbo.inclusion SET severity = @p1, position = @p2 WHERE inclusion_id = @p3`,
		[]any{int64(g.rng.Intn(6)), int64(g.rng.Intn(10)), iid}}}, func() {}}
}

func (g *Stones) opMoveInclusion() *op {
	iid, ok := g.pick(keys64(g.inclusions))
	sid, ok2 := g.pickAnyStone()
	if !ok || !ok2 {
		return nil
	}
	return &op{"move inclusion", []stmt{{`UPDATE dbo.inclusion SET stoneid = @p1 WHERE inclusion_id = @p2`, []any{sid, iid}}}, func() { g.inclusions[iid] = sid }}
}

func (g *Stones) opDeleteInclusion() *op {
	iid, ok := g.pick(keys64(g.inclusions))
	if !ok {
		return nil
	}
	return &op{"delete inclusion", []stmt{{`DELETE FROM dbo.inclusion WHERE inclusion_id = @p1`, []any{iid}}}, func() { delete(g.inclusions, iid) }}
}

func (g *Stones) opRekeyInclusion() *op {
	iid, ok := g.pick(keys64(g.inclusions))
	if !ok {
		return nil
	}
	nid := g.id()
	return &op{"change inclusion primary key", []stmt{{`UPDATE dbo.inclusion SET inclusion_id = @p1 WHERE inclusion_id = @p2`, []any{nid, iid}}}, func() {
		g.inclusions[nid] = g.inclusions[iid]
		delete(g.inclusions, iid)
	}}
}

func (g *Stones) opInsertRate() *op {
	sid, ok := g.pickAnyStone()
	if !ok {
		return nil
	}
	rid := g.id()
	return &op{"insert rate", []stmt{{`INSERT INTO dbo.rate (rate_id, stoneid, rate_type, amount, currency) VALUES (@p1, @p2, @p3, @p4, @p5)`,
		[]any{rid, sid, g.one([]string{"list", "cost", "offer"}), fmt.Sprintf("%d.%02d", 100+g.rng.Intn(9000), g.rng.Intn(100)), "USD"}}}, func() { g.rates[rid] = sid }}
}

func (g *Stones) opUpdateRate() *op {
	rid, ok := g.pick(keys64(g.rates))
	if !ok {
		return nil
	}
	return &op{"update rate", []stmt{{`UPDATE dbo.rate SET amount = @p1 WHERE rate_id = @p2`, []any{fmt.Sprintf("%d.%02d", 100+g.rng.Intn(9000), g.rng.Intn(100)), rid}}}, func() {}}
}

func (g *Stones) opMoveRate() *op {
	rid, ok := g.pick(keys64(g.rates))
	sid, ok2 := g.pickAnyStone()
	if !ok || !ok2 {
		return nil
	}
	return &op{"move rate", []stmt{{`UPDATE dbo.rate SET stoneid = @p1 WHERE rate_id = @p2`, []any{sid, rid}}}, func() { g.rates[rid] = sid }}
}

func (g *Stones) opDeleteRate() *op {
	rid, ok := g.pick(keys64(g.rates))
	if !ok {
		return nil
	}
	return &op{"delete rate", []stmt{{`DELETE FROM dbo.rate WHERE rate_id = @p1`, []any{rid}}}, func() { delete(g.rates, rid) }}
}

func (g *Stones) opRenameLab() *op {
	lid, ok := g.pickLabID()
	if !ok {
		return nil
	}
	return &op{"rename lab", []stmt{{`UPDATE dbo.lab SET name = @p1 WHERE lab_id = @p2`, []any{fmt.Sprintf("Lab %d rev %d", lid, g.rng.Intn(1000)), lid}}}, func() {}}
}

func (g *Stones) opRecodeLab() *op {
	lid, ok := g.pickLabID()
	if !ok {
		return nil
	}
	code := fmt.Sprintf("C%d", g.id())
	return &op{"change lab code", []stmt{{`UPDATE dbo.lab SET code = @p1 WHERE lab_id = @p2`, []any{code, lid}}}, func() { g.labs[lid] = code }}
}

func (g *Stones) opDeleteLab() *op {
	lid, ok := g.pickLabID()
	if !ok || len(g.labs) < 3 {
		return nil
	}
	return &op{"delete lab", []stmt{{`DELETE FROM dbo.lab WHERE lab_id = @p1`, []any{lid}}}, func() { delete(g.labs, lid) }}
}

func (g *Stones) opInsertLab() *op {
	lid := g.id()
	code := fmt.Sprintf("ZZ%d", g.rng.Intn(5))
	for _, c := range g.labs {
		if c == code {
			code = fmt.Sprintf("N%d", lid)
		}
	}
	return &op{"insert lab", []stmt{{`INSERT INTO dbo.lab (lab_id, code, name, country) VALUES (@p1, @p2, @p3, NULL)`, []any{lid, code, fmt.Sprintf("Lab %d", lid)}}}, func() { g.labs[lid] = code }}
}

func (g *Stones) opInsertOrder() *op {
	oid := g.id()
	return &op{"insert order", []stmt{{`INSERT INTO dbo.order_header (order_id, party, order_date, status) VALUES (@p1, @p2, @p3, @p4)`,
		[]any{oid, fmt.Sprintf("Party %d", g.rng.Intn(50)), time.Date(2026, 11, 1+g.rng.Intn(28), 0, 0, 0, 0, time.UTC), "open"}}}, func() { g.orders[oid] = true }}
}

func (g *Stones) opUpdateOrder() *op {
	oid, ok := g.pickOrder()
	if !ok {
		return nil
	}
	return &op{"update order", []stmt{{`UPDATE dbo.order_header SET status = @p1, party = @p2 WHERE order_id = @p3`,
		[]any{g.one([]string{"open", "confirmed", "shipped"}), fmt.Sprintf("Party %d", g.rng.Intn(50)), oid}}}, func() {}}
}

func (g *Stones) opDeleteOrder() *op {
	oid, ok := g.pickOrder()
	if !ok {
		return nil
	}
	return &op{"delete order", []stmt{{`DELETE FROM dbo.order_header WHERE order_id = @p1`, []any{oid}}}, func() { delete(g.orders, oid) }}
}

func (g *Stones) linkKeys() [][2]int64 {
	out := make([][2]int64, 0, len(g.links))
	for k := range g.links {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}

func (g *Stones) opLink() *op {
	oid, ok := g.pick(append(keys64(g.orders), 999)) // 999: an order id that does not exist
	sid, ok2 := g.pickAnyStone()
	if !ok || !ok2 || g.links[[2]int64{oid, sid}] {
		return nil
	}
	return &op{"link stone to order", []stmt{{`INSERT INTO dbo.order_stone (order_id, stoneid) VALUES (@p1, @p2)`, []any{oid, sid}}}, func() { g.links[[2]int64{oid, sid}] = true }}
}

func (g *Stones) opUnlink() *op {
	ks := g.linkKeys()
	if len(ks) == 0 {
		return nil
	}
	k := ks[g.rng.Intn(len(ks))]
	return &op{"unlink stone from order", []stmt{{`DELETE FROM dbo.order_stone WHERE order_id = @p1 AND stoneid = @p2`, []any{k[0], k[1]}}}, func() { delete(g.links, k) }}
}

func (g *Stones) opMoveLink() *op {
	ks := g.linkKeys()
	sid, ok := g.pickAnyStone()
	if len(ks) == 0 || !ok {
		return nil
	}
	k := ks[g.rng.Intn(len(ks))]
	nk := [2]int64{k[0], sid}
	if g.links[nk] {
		return nil
	}
	return &op{"move order link (primary key update)", []stmt{{`UPDATE dbo.order_stone SET stoneid = @p1 WHERE order_id = @p2 AND stoneid = @p3`, []any{sid, k[0], k[1]}}}, func() {
		delete(g.links, k)
		g.links[nk] = true
	}}
}

func (g *Stones) opInsertLine() *op {
	oid, ok := g.pickOrder()
	if !ok {
		return nil
	}
	lid := g.id()
	return &op{"insert order line", []stmt{{`INSERT INTO dbo.order_line (line_id, order_id, note, amount) VALUES (@p1, @p2, @p3, @p4)`,
		[]any{lid, oid, "line", fmt.Sprintf("%d.%02d", 1+g.rng.Intn(900), g.rng.Intn(100))}}}, func() { g.lines[lid] = oid }}
}

func (g *Stones) opUpdateLine() *op {
	lid, ok := g.pick(keys64(g.lines))
	if !ok {
		return nil
	}
	return &op{"update order line", []stmt{{`UPDATE dbo.order_line SET amount = @p1 WHERE line_id = @p2`, []any{fmt.Sprintf("%d.%02d", 1+g.rng.Intn(900), g.rng.Intn(100)), lid}}}, func() {}}
}

func (g *Stones) opMoveLine() *op {
	lid, ok := g.pick(keys64(g.lines))
	oid, ok2 := g.pickOrder()
	if !ok || !ok2 {
		return nil
	}
	return &op{"move order line", []stmt{{`UPDATE dbo.order_line SET order_id = @p1 WHERE line_id = @p2`, []any{oid, lid}}}, func() { g.lines[lid] = oid }}
}

func (g *Stones) opDeleteLine() *op {
	lid, ok := g.pick(keys64(g.lines))
	if !ok {
		return nil
	}
	return &op{"delete order line", []stmt{{`DELETE FROM dbo.order_line WHERE line_id = @p1`, []any{lid}}}, func() { delete(g.lines, lid) }}
}

// opBurst touches many stones at once, exceeding small budgets and fan-out limits.
func (g *Stones) opBurst() *op {
	if g.rng.Intn(2) == 0 {
		lid, ok := g.pickLabID()
		if !ok {
			return nil
		}
		ids := keys64(g.stones)
		g.rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		if len(ids) > 40 {
			ids = ids[:40]
		}
		if len(ids) == 0 {
			return nil
		}
		var ph []string
		args := []any{g.labs[lid]}
		for i, id := range ids {
			ph = append(ph, fmt.Sprintf("@p%d", i+2))
			args = append(args, id)
		}
		return &op{"burst: move 40 stones to one lab", []stmt{{`UPDATE dbo.stone SET lab_code = @p1 WHERE stoneid IN (` + strings.Join(ph, ",") + `)`, args}}, func() {}}
	}
	// Rename the most used lab: a fan-out across many stones.
	return &op{"burst: rename every lab", []stmt{{`UPDATE dbo.lab SET name = name + N'*'`, nil}}, func() {}}
}
