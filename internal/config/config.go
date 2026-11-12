// Package config loads, defaults and validates docsync configuration files.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Duration is a time.Duration that reads and writes as a Go duration string ("2s", "15m").
type Duration struct{ time.Duration }

// MarshalJSON renders the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Duration.String()) }

// UnmarshalJSON accepts a duration string such as "1500ms" or "2s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"2s\", got %s", string(b))
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// Config is the root of a docsync configuration file.
type Config struct {
	Source    Source        `json:"source"`
	Document  Document      `json:"document"`
	Routing   []RoutingRule `json:"routing"`
	Sinks     []Sink        `json:"sinks"`
	State     State         `json:"state"`
	Dashboard Dashboard     `json:"dashboard"`

	// path is the file the config was loaded from; relative directories resolve against it.
	path string
}

// Source describes the database documents are assembled from.
type Source struct {
	Type   string `json:"type"`
	DSN    string `json:"dsn,omitempty"`
	DSNEnv string `json:"dsn_env,omitempty"`
	// PollInterval is how long an idle engine waits before checking for changes again.
	PollInterval Duration `json:"poll_interval,omitempty"`
	// AssembleBudget caps how many documents one cycle assembles.
	AssembleBudget int `json:"assemble_budget,omitempty"`
	// LowPriorityShare is the fraction of the budget reserved for rebuild, full-load
	// and sweeper work whenever such work is queued, so it always makes progress.
	LowPriorityShare float64 `json:"low_priority_share,omitempty"`
	// MaxCycleRetries bounds retries of a cycle after transient SQL errors.
	MaxCycleRetries int `json:"max_cycle_retries,omitempty"`
	// SweepPerMinute re-checks this many stored documents per minute against the
	// database (0 = off). Differences mean a change was missed and are corrected.
	SweepPerMinute int `json:"sweep_per_minute,omitempty"`
}

// Document describes the document shape: one document per root row, built from fragments.
type Document struct {
	Name      string     `json:"name"`
	Root      Root       `json:"root"`
	Fragments []Fragment `json:"fragments"`
}

// Root identifies the table whose rows are the documents.
type Root struct {
	Table string   `json:"table"`
	Key   []string `json:"key"`
	// Filter is an optional SQL condition on the root table (alias t, or bare column
	// names). Only rows that match become documents; a row that stops matching is
	// deleted from the output, and one that starts matching is added. It may use only
	// the root table's own columns, so that every change to it is tracked.
	Filter string `json:"filter,omitempty"`
}

// Fragment is one SQL query contributing fields to documents. The query must join the
// {{roots}} placeholder and select its id column as _root.
type Fragment struct {
	Name        string   `json:"name"`
	Cardinality string   `json:"cardinality,omitempty"` // "one" (default) or "many"
	Path        string   `json:"path,omitempty"`        // many: list field name; one: optional struct field name
	SortKey     []string `json:"sort_key,omitempty"`    // many: required, defines list order
	SQL         string   `json:"sql"`
}

// Cardinality values.
const (
	CardinalityOne  = "one"
	CardinalityMany = "many"
)

// RoutingRule tells docsync how changes in one table reach root documents.
type RoutingRule struct {
	Table          string   `json:"table"`
	Key            []string `json:"key,omitempty"`
	Next           *Link    `json:"next,omitempty"`
	Static         bool     `json:"static,omitempty"`
	HotFanoutLimit int      `json:"hot_fanout_limit,omitempty"`
}

// Link connects a routing table to the next table on its path to the root.
type Link struct {
	Table string `json:"table"`
	// Join maps columns of this table to columns of the next table.
	Join map[string]string `json:"join"`
	// Kind is "references" (this row holds the next table's columns) or
	// "referenced_by" (rows of the next table hold this table's columns).
	Kind string `json:"kind"`
}

// Link kinds.
const (
	KindReferences   = "references"
	KindReferencedBy = "referenced_by"
)

// Sink is an output destination for documents.
type Sink struct {
	Name            string   `json:"name,omitempty"`
	Type            string   `json:"type"`
	Dir             string   `json:"dir"`
	SegmentInterval Duration `json:"segment_interval,omitempty"`
	BaseEvery       Duration `json:"base_every,omitempty"`
	RetainSegments  Duration `json:"retain_segments,omitempty"`
	IncludeJSON     bool     `json:"include_json,omitempty"`
	BaseFileRows    int      `json:"base_file_rows,omitempty"`
}

// Sink types.
const (
	SinkParquetLog = "parquet_log"
	SinkNDJSON     = "ndjson"
)

// State configures the local state store.
type State struct {
	Dir             string   `json:"dir"`
	OutboxRetention Duration `json:"outbox_retention,omitempty"`
}

// Dashboard configures the embedded web dashboard and admin API.
type Dashboard struct {
	Listen               string `json:"listen,omitempty"`
	Disabled             bool   `json:"disabled,omitempty"`
	BasicAuthUser        string `json:"basic_auth_user,omitempty"`
	BasicAuthPasswordEnv string `json:"basic_auth_password_env,omitempty"`
}

// Load reads, defaults and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg.path = abs
	return cfg, nil
}

// Parse decodes YAML (or JSON), applies defaults and validates.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Source.PollInterval.Duration == 0 {
		c.Source.PollInterval.Duration = time.Second
	}
	if c.Source.AssembleBudget == 0 {
		c.Source.AssembleBudget = 5000
	}
	if c.Source.LowPriorityShare == 0 {
		c.Source.LowPriorityShare = 0.2
	}
	if c.Source.MaxCycleRetries == 0 {
		c.Source.MaxCycleRetries = 5
	}
	for i := range c.Document.Fragments {
		f := &c.Document.Fragments[i]
		if f.Cardinality == "" {
			f.Cardinality = CardinalityOne
		}
	}
	for i := range c.Sinks {
		s := &c.Sinks[i]
		if s.Name == "" {
			s.Name = s.Type
			if i > 0 {
				s.Name = fmt.Sprintf("%s-%d", s.Type, i)
			}
		}
		if s.SegmentInterval.Duration == 0 {
			s.SegmentInterval.Duration = 2 * time.Second
		}
		if s.BaseEvery.Duration == 0 {
			s.BaseEvery.Duration = 15 * time.Minute
		}
		if s.RetainSegments.Duration == 0 {
			s.RetainSegments.Duration = time.Hour
		}
		if s.BaseFileRows == 0 {
			s.BaseFileRows = 500_000
		}
	}
	if c.State.Dir == "" {
		c.State.Dir = "./state"
	}
	if c.State.OutboxRetention.Duration == 0 {
		c.State.OutboxRetention.Duration = 24 * time.Hour
	}
	if c.Dashboard.Listen == "" {
		c.Dashboard.Listen = "127.0.0.1:8088"
	}
}

// ResolvePath turns a path from the config into an absolute path, relative to the config file.
func (c *Config) ResolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) || c.path == "" {
		return p
	}
	return filepath.Join(filepath.Dir(c.path), p)
}

// DSNValue returns the connection string, reading DSNEnv when set.
func (c *Config) DSNValue() (string, error) {
	if c.Source.DSNEnv != "" {
		v := os.Getenv(c.Source.DSNEnv)
		if v == "" {
			return "", fmt.Errorf("environment variable %s (source.dsn_env) is empty", c.Source.DSNEnv)
		}
		return v, nil
	}
	if c.Source.DSN == "" {
		return "", fmt.Errorf("set source.dsn_env (recommended) or source.dsn")
	}
	return c.Source.DSN, nil
}

// ContentHash fingerprints everything that affects document content or routing.
// A change means stored documents may be stale, so the engine resnapshots.
func (c *Config) ContentHash() string {
	b, _ := json.Marshal(struct {
		D Document      `json:"d"`
		R []RoutingRule `json:"r"`
	}{c.Document, c.Routing})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Fragment returns the fragment with the given name.
func (c *Config) Fragment(name string) (Fragment, bool) {
	for _, f := range c.Document.Fragments {
		if f.Name == name {
			return f, true
		}
	}
	return Fragment{}, false
}

// RootTable returns the parsed root table name.
func (c *Config) RootTable() TableName { return MustParseTableName(c.Document.Root.Table) }

// TableName is a schema-qualified SQL Server table name.
type TableName struct {
	Schema string
	Name   string
}

// ParseTableName accepts "table", "schema.table" or "[schema].[table]".
func ParseTableName(s string) (TableName, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return TableName{}, fmt.Errorf("empty table name")
	}
	parts := splitIdent(s)
	switch len(parts) {
	case 1:
		return TableName{Schema: "dbo", Name: parts[0]}, nil
	case 2:
		return TableName{Schema: parts[0], Name: parts[1]}, nil
	default:
		return TableName{}, fmt.Errorf("table name %q must be table or schema.table (three-part names are not supported)", s)
	}
}

// MustParseTableName parses a table name already checked by Validate.
func MustParseTableName(s string) TableName {
	t, err := ParseTableName(s)
	if err != nil {
		panic(err)
	}
	return t
}

func splitIdent(s string) []string {
	var parts []string
	var cur strings.Builder
	inBracket := false
	for _, r := range s {
		switch {
		case r == '[' && !inBracket:
			inBracket = true
		case r == ']' && inBracket:
			inBracket = false
		case r == '.' && !inBracket:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	parts = append(parts, cur.String())
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// Quoted renders the name as [schema].[table].
func (t TableName) Quoted() string { return QuoteIdent(t.Schema) + "." + QuoteIdent(t.Name) }

// String renders the name as schema.table.
func (t TableName) String() string { return t.Schema + "." + t.Name }

// Key is a case-insensitive identity for maps.
func (t TableName) Key() string { return strings.ToLower(t.String()) }

// QuoteIdent brackets an identifier, escaping closing brackets.
func QuoteIdent(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }
