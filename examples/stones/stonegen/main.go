// Command stonegen creates the example stone database and keeps changing it, for
// demos and load tests.
//
//	DOCSYNC_DSN='sqlserver://sa:...@localhost:1433?database=master' \
//	  go run ./examples/stones/stonegen --create-database stones --stones 10000 --churn 1000
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/Jayx282/docsync/examples/stones"
	"github.com/Jayx282/docsync/internal/testkit"
)

func main() {
	dsnEnv := flag.String("dsn-env", "DOCSYNC_DSN", "environment variable with the connection string")
	createDB := flag.String("create-database", "", "create this database, apply the example schema and seed it")
	nStones := flag.Int("stones", 1000, "stones to seed when creating the database")
	churn := flag.Int("churn", 0, "random transactions per minute to apply (0 = none)")
	dur := flag.Duration("for", 0, "stop churning after this long (0 = until interrupted)")
	seed := flag.Int64("seed", time.Now().UnixNano(), "random seed")
	flag.Parse()

	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		log.Fatalf("set %s", *dsnEnv)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	shown := "" // the new DSN with its password hidden, for the message below
	if *createDB != "" {
		master, err := sql.Open("sqlserver", dsn)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := master.ExecContext(ctx, "CREATE DATABASE ["+strings.ReplaceAll(*createDB, "]", "]]")+"]"); err != nil {
			log.Fatalf("create database: %v", err)
		}
		master.Close()
		u, err := url.Parse(dsn)
		if err != nil {
			log.Fatal(err)
		}
		q := u.Query()
		q.Set("database", *createDB)
		u.RawQuery = q.Encode()
		dsn = u.String()
		shown = u.Redacted()
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	gen := testkit.NewStones(db, *seed)
	if *createDB != "" {
		for _, batch := range testkit.SplitBatches(string(stones.Schema)) {
			if _, err := db.ExecContext(ctx, batch); err != nil {
				log.Fatalf("schema: %v", err)
			}
		}
		start := time.Now()
		if err := gen.Seed(ctx, *nStones, 12, *nStones/4); err != nil {
			log.Fatalf("seed: %v", err)
		}
		fmt.Printf("created %s with %d stones in %s\nuse it with: %s\n", *createDB, *nStones, time.Since(start).Round(time.Millisecond), shown)
	}
	if *churn <= 0 {
		return
	}
	if *createDB == "" {
		log.Fatal("--churn needs a database created by this tool in the same run (it tracks row ids in memory)")
	}
	tick := time.NewTicker(time.Minute / time.Duration(*churn))
	defer tick.Stop()
	var end <-chan time.Time
	if *dur > 0 {
		end = time.After(*dur)
	}
	n := 0
	report := time.NewTicker(10 * time.Second)
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("stopped after %d transactions\n", n)
			return
		case <-end:
			fmt.Printf("done: %d transactions\n", n)
			return
		case <-report.C:
			fmt.Printf("%d transactions so far\n", n)
		case <-tick.C:
			if _, err := gen.RandomTransaction(ctx, 3); err != nil {
				log.Printf("transaction failed: %v", err)
				continue
			}
			n++
		}
	}
}
