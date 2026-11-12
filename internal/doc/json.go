package doc

import (
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// JSON renders v as canonical JSON: object keys sorted, no insignificant whitespace,
// decimals as exact numbers, dates and times as ISO-8601 strings, bytes as base64.
func JSON(v any) ([]byte, error) {
	var sb strings.Builder
	if err := writeJSON(&sb, v); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// MustJSON is JSON for values produced by this package.
func MustJSON(v any) string {
	b, err := JSON(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeJSON(sb *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		sb.WriteString("null")
	case bool:
		sb.WriteString(strconv.FormatBool(x))
	case int64:
		sb.WriteString(strconv.FormatInt(x, 10))
	case float64:
		switch {
		case math.IsNaN(x):
			sb.WriteString(`"NaN"`)
		case math.IsInf(x, 1):
			sb.WriteString(`"Infinity"`)
		case math.IsInf(x, -1):
			sb.WriteString(`"-Infinity"`)
		default:
			sb.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
		}
	case string:
		writeString(sb, x)
	case Decimal:
		sb.WriteString(string(x))
	case UUID:
		writeString(sb, string(x))
	case Date:
		writeString(sb, x.Time().Format("2006-01-02"))
	case Timestamp:
		writeString(sb, formatClock(x.Time().Format("2006-01-02T15:04:05"), x.Time().Nanosecond()))
	case TimestampTZ:
		writeString(sb, formatClock(x.Time().Format("2006-01-02T15:04:05"), x.Time().Nanosecond())+"Z")
	case TimeOfDay:
		us := int64(x)
		h, m, s := us/3_600_000_000, us/60_000_000%60, us/1_000_000%60
		writeString(sb, formatClock(fmt.Sprintf("%02d:%02d:%02d", h, m, s), int(us%1_000_000)*1000))
	case []byte:
		writeString(sb, base64.StdEncoding.EncodeToString(x))
	case Object:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeString(sb, k)
			sb.WriteByte(':')
			if err := writeJSON(sb, x[k]); err != nil {
				return err
			}
		}
		sb.WriteByte('}')
	case List:
		sb.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				sb.WriteByte(',')
			}
			if err := writeJSON(sb, e); err != nil {
				return err
			}
		}
		sb.WriteByte(']')
	default:
		return fmt.Errorf("doc: unsupported value type %T", v)
	}
	return nil
}

// formatClock appends a fractional-seconds part with trailing zeros trimmed.
func formatClock(base string, nanos int) string {
	if nanos == 0 {
		return base
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", nanos/1000), "0")
	if frac == "" {
		return base
	}
	return base + "." + frac
}

const hexDigits = "0123456789abcdef"

func writeString(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"':
				sb.WriteString(`\"`)
			case c == '\\':
				sb.WriteString(`\\`)
			case c == '\n':
				sb.WriteString(`\n`)
			case c == '\r':
				sb.WriteString(`\r`)
			case c == '\t':
				sb.WriteString(`\t`)
			case c < 0x20:
				sb.WriteString(`\u00`)
				sb.WriteByte(hexDigits[c>>4])
				sb.WriteByte(hexDigits[c&0xF])
			default:
				sb.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			sb.WriteString(`�`)
		} else {
			sb.WriteString(s[i : i+size])
		}
		i += size
	}
	sb.WriteByte('"')
}
