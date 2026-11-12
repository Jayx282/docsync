// Package schema derives the document schema from fragment result columns and
// converts raw database values into typed document values.
package schema

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/doc"
)

// Kind is a logical value type.
type Kind string

// Kinds.
const (
	Bool        Kind = "bool"
	UInt8       Kind = "uint8"
	Int16       Kind = "int16"
	Int32       Kind = "int32"
	Int64       Kind = "int64"
	Float32     Kind = "float32"
	Float64     Kind = "float64"
	Decimal     Kind = "decimal"
	String      Kind = "string"
	UUID        Kind = "uuid"
	Date        Kind = "date"
	Timestamp   Kind = "timestamp"
	TimestampTZ Kind = "timestamptz"
	Time        Kind = "time"
	Binary      Kind = "binary"
	Struct      Kind = "struct"
	List        Kind = "list"
)

// Type is a logical type, possibly nested.
type Type struct {
	Kind      Kind    `json:"kind"`
	Precision int32   `json:"precision,omitempty"`
	Scale     int32   `json:"scale,omitempty"`
	Fields    []Field `json:"fields,omitempty"`
	Elem      *Type   `json:"elem,omitempty"`
	SQLType   string  `json:"sql_type,omitempty"`
}

// Field is a named type.
type Field struct {
	Name string `json:"name"`
	Type Type   `json:"type"`
}

// Schema describes every document produced for one configuration epoch.
type Schema struct {
	Document string  `json:"document"`
	RootKey  Field   `json:"root_key"`
	Fields   []Field `json:"fields"`
}

// Column describes one result column reported by the database.
type Column struct {
	Name      string
	DBType    string // e.g. INT, NVARCHAR, DECIMAL, DATETIME2
	Precision int64
	Scale     int64
}

// FragmentColumns pairs a fragment definition with its result columns.
type FragmentColumns struct {
	Fragment config.Fragment
	Columns  []Column
}

// TypeForSQL maps a SQL Server type name to a logical type.
func TypeForSQL(c Column) (Type, error) {
	name := strings.ToUpper(c.DBType)
	t := Type{SQLType: strings.ToLower(name)}
	switch name {
	case "BIT":
		t.Kind = Bool
	case "TINYINT":
		t.Kind = UInt8
	case "SMALLINT":
		t.Kind = Int16
	case "INT":
		t.Kind = Int32
	case "BIGINT":
		t.Kind = Int64
	case "REAL":
		t.Kind = Float32
	case "FLOAT":
		t.Kind = Float64
	case "DECIMAL", "NUMERIC":
		t.Kind, t.Precision, t.Scale = Decimal, int32(c.Precision), int32(c.Scale)
		if t.Precision <= 0 || t.Precision > 38 {
			return Type{}, fmt.Errorf("column %s: decimal precision %d is out of range", c.Name, c.Precision)
		}
	case "MONEY":
		t.Kind, t.Precision, t.Scale = Decimal, 19, 4
	case "SMALLMONEY":
		t.Kind, t.Precision, t.Scale = Decimal, 10, 4
	case "CHAR", "VARCHAR", "NCHAR", "NVARCHAR", "TEXT", "NTEXT", "XML", "SYSNAME":
		t.Kind = String
	case "UNIQUEIDENTIFIER":
		t.Kind = UUID
	case "DATE":
		t.Kind = Date
	case "DATETIME", "DATETIME2", "SMALLDATETIME":
		t.Kind = Timestamp
	case "DATETIMEOFFSET":
		t.Kind = TimestampTZ
	case "TIME":
		t.Kind = Time
	case "BINARY", "VARBINARY", "IMAGE", "TIMESTAMP", "ROWVERSION":
		t.Kind = Binary
	default:
		return Type{}, fmt.Errorf("column %s: SQL type %s is not supported; CAST it in the fragment SQL (for example to nvarchar(max))", c.Name, c.DBType)
	}
	return t, nil
}

// Build derives the document schema from fragment result columns.
func Build(docName string, rootKeyName string, rootKey Column, frags []FragmentColumns) (*Schema, error) {
	rk, err := TypeForSQL(rootKey)
	if err != nil {
		return nil, fmt.Errorf("root key: %w", err)
	}
	if !keyable(rk.Kind) {
		return nil, fmt.Errorf("root key type %s cannot be used as a document key", rk.SQLType)
	}
	s := &Schema{Document: docName, RootKey: Field{Name: rootKeyName, Type: rk}}
	seen := map[string]string{}
	claim := func(name, owner string) error {
		k := strings.ToLower(name)
		if prev, ok := seen[k]; ok {
			return fmt.Errorf("field %q is produced by both %s and %s; rename one with AS", name, prev, owner)
		}
		seen[k] = owner
		return nil
	}
	for _, fc := range frags {
		f := fc.Fragment
		owner := "fragment " + f.Name
		var cols []Field
		rootFound := false
		colSeen := map[string]bool{}
		for _, c := range fc.Columns {
			if strings.EqualFold(c.Name, config.RootColumn) {
				rootFound = true
				continue
			}
			if c.Name == "" {
				return nil, fmt.Errorf("%s: every column needs a name (use AS)", owner)
			}
			if colSeen[strings.ToLower(c.Name)] {
				return nil, fmt.Errorf("%s: duplicate column %q", owner, c.Name)
			}
			colSeen[strings.ToLower(c.Name)] = true
			t, err := TypeForSQL(c)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", owner, err)
			}
			cols = append(cols, Field{Name: c.Name, Type: t})
		}
		if !rootFound {
			return nil, fmt.Errorf("%s: the query must select the roots id column AS %s", owner, config.RootColumn)
		}
		for _, k := range f.SortKey {
			if !colSeen[strings.ToLower(k)] {
				return nil, fmt.Errorf("%s: sort_key column %q is not selected by the query", owner, k)
			}
		}
		switch {
		case f.Cardinality == config.CardinalityMany:
			if err := claim(f.Path, owner); err != nil {
				return nil, err
			}
			elem := Type{Kind: Struct, Fields: cols}
			s.Fields = append(s.Fields, Field{Name: f.Path, Type: Type{Kind: List, Elem: &elem}})
		case f.Path != "":
			if err := claim(f.Path, owner); err != nil {
				return nil, err
			}
			s.Fields = append(s.Fields, Field{Name: f.Path, Type: Type{Kind: Struct, Fields: cols}})
		default:
			for _, c := range cols {
				if err := claim(c.Name, owner); err != nil {
					return nil, err
				}
				s.Fields = append(s.Fields, c)
			}
		}
	}
	return s, nil
}

func keyable(k Kind) bool {
	switch k {
	case UInt8, Int16, Int32, Int64, String, UUID, Date, Decimal:
		return true
	}
	return false
}

// Hash fingerprints the schema.
func (s *Schema) Hash() string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Convert turns a raw database/sql value into the typed document value for t.
func Convert(t Type, raw any) (any, error) {
	if raw == nil {
		return nil, nil
	}
	switch t.Kind {
	case Bool:
		switch v := raw.(type) {
		case bool:
			return v, nil
		case int64:
			return v != 0, nil
		}
	case UInt8, Int16, Int32, Int64:
		switch v := raw.(type) {
		case int64:
			return v, nil
		case int32:
			return int64(v), nil
		case int16:
			return int64(v), nil
		case uint8:
			return int64(v), nil
		case int:
			return int64(v), nil
		}
	case Float32, Float64:
		switch v := raw.(type) {
		case float64:
			if t.Kind == Float32 {
				return float64(float32(v)), nil
			}
			return v, nil
		case float32:
			return float64(v), nil
		case int64:
			return float64(v), nil
		}
	case Decimal:
		switch v := raw.(type) {
		case []byte:
			return doc.NormalizeDecimal(string(v), int(t.Scale))
		case string:
			return doc.NormalizeDecimal(v, int(t.Scale))
		case float64:
			return doc.NormalizeDecimal(fmt.Sprintf("%.*f", t.Scale, v), int(t.Scale))
		case int64:
			return doc.NormalizeDecimal(fmt.Sprint(v), int(t.Scale))
		}
	case String:
		switch v := raw.(type) {
		case string:
			return v, nil
		case []byte:
			return string(v), nil
		}
	case UUID:
		switch v := raw.(type) {
		case []byte:
			if len(v) != 16 {
				return nil, fmt.Errorf("uniqueidentifier must be 16 bytes, got %d", len(v))
			}
			return doc.UUID(formatSQLGUID(v)), nil
		case string:
			return doc.UUID(strings.ToUpper(v)), nil
		}
	case Date:
		if v, ok := raw.(time.Time); ok {
			return doc.DateOf(v), nil
		}
	case Timestamp:
		if v, ok := raw.(time.Time); ok {
			return doc.TimestampOf(v), nil
		}
	case TimestampTZ:
		if v, ok := raw.(time.Time); ok {
			return doc.TimestampTZOf(v), nil
		}
	case Time:
		if v, ok := raw.(time.Time); ok {
			return doc.TimeOfDayOf(v), nil
		}
	case Binary:
		if v, ok := raw.([]byte); ok {
			return append([]byte(nil), v...), nil
		}
	}
	return nil, fmt.Errorf("cannot convert %T to %s", raw, t.Kind)
}

// formatSQLGUID renders SQL Server's mixed-endian GUID bytes in canonical form.
func formatSQLGUID(b []byte) string {
	d1 := binary.LittleEndian.Uint32(b[0:4])
	d2 := binary.LittleEndian.Uint16(b[4:6])
	d3 := binary.LittleEndian.Uint16(b[6:8])
	return strings.ToUpper(fmt.Sprintf("%08x-%04x-%04x-%x-%x", d1, d2, d3, b[8:10], b[10:16]))
}

// FromKeyPrimitive reverses doc.KeyPrimitive for a value of type t.
func FromKeyPrimitive(t Type, p any) (any, error) {
	if p == nil {
		return nil, nil
	}
	switch t.Kind {
	case UInt8, Int16, Int32, Int64:
		if v, ok := p.(int64); ok {
			return v, nil
		}
	case Bool:
		if v, ok := p.(bool); ok {
			return v, nil
		}
	case String:
		if v, ok := p.(string); ok {
			return v, nil
		}
	case UUID:
		if v, ok := p.(string); ok {
			return doc.UUID(v), nil
		}
	case Decimal:
		if v, ok := p.(string); ok {
			return doc.Decimal(v), nil
		}
	case Date:
		if v, ok := p.(int64); ok {
			return doc.Date(v), nil
		}
	case Timestamp:
		if v, ok := p.(int64); ok {
			return doc.Timestamp(v), nil
		}
	case TimestampTZ:
		if v, ok := p.(int64); ok {
			return doc.TimestampTZ(v), nil
		}
	case Time:
		if v, ok := p.(int64); ok {
			return doc.TimeOfDay(v), nil
		}
	case Binary:
		if v, ok := p.([]byte); ok {
			return v, nil
		}
	}
	return nil, fmt.Errorf("key value %T does not match type %s", p, t.Kind)
}

// FieldByName finds a top-level field, case-insensitively.
func (s *Schema) FieldByName(name string) (Field, bool) {
	for _, f := range s.Fields {
		if strings.EqualFold(f.Name, name) {
			return f, true
		}
	}
	return Field{}, false
}
