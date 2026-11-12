// Package doc defines the typed values documents are made of, plus deterministic
// binary and JSON encodings used for storage, hashing and output.
//
// A document is an Object. Values are one of:
//
//	nil, bool, int64, float64, string, Decimal, UUID, Date, Timestamp,
//	TimestampTZ, TimeOfDay, []byte, Object, List
package doc

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Decimal is an exact decimal number kept as text, e.g. "-12.340".
type Decimal string

// UUID is a GUID in canonical upper-case form, as SQL Server renders it.
type UUID string

// Date is a calendar date, as days since 1970-01-01.
type Date int32

// Timestamp is a wall-clock date and time without a zone, in microseconds since
// 1970-01-01 00:00:00 (SQL Server datetime, datetime2, smalldatetime).
type Timestamp int64

// TimestampTZ is an instant in microseconds since the Unix epoch, UTC
// (SQL Server datetimeoffset, normalized to UTC).
type TimestampTZ int64

// TimeOfDay is a time of day in microseconds since midnight.
type TimeOfDay int64

// Object is a document or nested struct.
type Object map[string]any

// List is an ordered list of values (usually Objects).
type List []any

var epoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// DateOf converts a time to a Date using its calendar fields.
func DateOf(t time.Time) Date {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return Date(d.Sub(epoch).Hours() / 24)
}

// Time returns the date at midnight UTC.
func (d Date) Time() time.Time { return epoch.AddDate(0, 0, int(d)) }

// TimestampOf converts a time to a zone-less Timestamp using its wall-clock fields.
func TimestampOf(t time.Time) Timestamp {
	wall := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	return Timestamp(wall.UnixMicro())
}

// Time returns the timestamp as a UTC time with the same wall clock.
func (t Timestamp) Time() time.Time { return time.UnixMicro(int64(t)).UTC() }

// TimestampTZOf converts a time to a UTC instant.
func TimestampTZOf(t time.Time) TimestampTZ { return TimestampTZ(t.UnixMicro()) }

// Time returns the instant in UTC.
func (t TimestampTZ) Time() time.Time { return time.UnixMicro(int64(t)).UTC() }

// TimeOfDayOf converts the clock part of a time.
func TimeOfDayOf(t time.Time) TimeOfDay {
	return TimeOfDay(int64(t.Hour())*3_600_000_000 + int64(t.Minute())*60_000_000 +
		int64(t.Second())*1_000_000 + int64(t.Nanosecond()/1000))
}

// NormalizeDecimal canonicalizes decimal text to exactly scale fractional digits.
func NormalizeDecimal(s string, scale int) (Decimal, error) {
	s = strings.TrimSpace(s)
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", fmt.Errorf("invalid decimal %q", s)
	}
	return Decimal(r.FloatString(scale)), nil
}

// KeyPrimitive converts a value to a primitive accepted by package keyenc.
func KeyPrimitive(v any) (any, error) {
	switch x := v.(type) {
	case nil, bool, int64, string, []byte:
		return x, nil
	case Decimal:
		return string(x), nil
	case UUID:
		return string(x), nil
	case Date:
		return int64(x), nil
	case Timestamp:
		return int64(x), nil
	case TimestampTZ:
		return int64(x), nil
	case TimeOfDay:
		return int64(x), nil
	default:
		return nil, fmt.Errorf("type %T cannot be used as a key", v)
	}
}

// TypeName names a value's type for messages.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case int64:
		return "int"
	case float64:
		return "float"
	case string:
		return "string"
	case Decimal:
		return "decimal"
	case UUID:
		return "uuid"
	case Date:
		return "date"
	case Timestamp:
		return "timestamp"
	case TimestampTZ:
		return "timestamptz"
	case TimeOfDay:
		return "time"
	case []byte:
		return "binary"
	case Object:
		return "object"
	case List:
		return "list"
	default:
		return fmt.Sprintf("%T", v)
	}
}
