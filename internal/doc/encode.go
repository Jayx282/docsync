package doc

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Binary encoding tags. The encoding is canonical: equal values always produce
// equal bytes (object keys are sorted), so encodings can be compared directly.
const (
	tNil byte = iota
	tFalse
	tTrue
	tInt
	tFloat
	tString
	tDecimal
	tDate
	tTimestamp
	tTimestampTZ
	tTime
	tUUID
	tBytes
	tObject
	tList
)

// Encode returns the canonical binary encoding of v.
func Encode(v any) ([]byte, error) {
	return AppendEncode(nil, v)
}

// MustEncode is Encode for values produced by this package.
func MustEncode(v any) []byte {
	b, err := Encode(v)
	if err != nil {
		panic(err)
	}
	return b
}

// AppendEncode appends the canonical encoding of v to dst.
func AppendEncode(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, tNil), nil
	case bool:
		if x {
			return append(dst, tTrue), nil
		}
		return append(dst, tFalse), nil
	case int64:
		return binary.AppendVarint(append(dst, tInt), x), nil
	case float64:
		return binary.BigEndian.AppendUint64(append(dst, tFloat), math.Float64bits(x)), nil
	case string:
		return appendStr(append(dst, tString), x), nil
	case Decimal:
		return appendStr(append(dst, tDecimal), string(x)), nil
	case UUID:
		return appendStr(append(dst, tUUID), string(x)), nil
	case Date:
		return binary.AppendVarint(append(dst, tDate), int64(x)), nil
	case Timestamp:
		return binary.AppendVarint(append(dst, tTimestamp), int64(x)), nil
	case TimestampTZ:
		return binary.AppendVarint(append(dst, tTimestampTZ), int64(x)), nil
	case TimeOfDay:
		return binary.AppendVarint(append(dst, tTime), int64(x)), nil
	case []byte:
		dst = binary.AppendUvarint(append(dst, tBytes), uint64(len(x)))
		return append(dst, x...), nil
	case Object:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = binary.AppendUvarint(append(dst, tObject), uint64(len(keys)))
		var err error
		for _, k := range keys {
			dst = appendStr(dst, k)
			if dst, err = AppendEncode(dst, x[k]); err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
		}
		return dst, nil
	case List:
		dst = binary.AppendUvarint(append(dst, tList), uint64(len(x)))
		var err error
		for i, e := range x {
			if dst, err = AppendEncode(dst, e); err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
		}
		return dst, nil
	default:
		return nil, fmt.Errorf("doc: unsupported value type %T", v)
	}
}

func appendStr(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// Decode decodes a value produced by Encode.
func Decode(b []byte) (any, error) {
	v, rest, err := decode(b)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("doc: %d trailing bytes", len(rest))
	}
	return v, nil
}

// DecodeObject decodes an encoded Object.
func DecodeObject(b []byte) (Object, error) {
	v, err := Decode(b)
	if err != nil {
		return nil, err
	}
	o, ok := v.(Object)
	if !ok {
		return nil, fmt.Errorf("doc: expected object, got %s", TypeName(v))
	}
	return o, nil
}

func decode(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, fmt.Errorf("doc: unexpected end of input")
	}
	tag, b := b[0], b[1:]
	switch tag {
	case tNil:
		return nil, b, nil
	case tFalse:
		return false, b, nil
	case tTrue:
		return true, b, nil
	case tInt, tDate, tTimestamp, tTimestampTZ, tTime:
		v, n := binary.Varint(b)
		if n <= 0 {
			return nil, nil, fmt.Errorf("doc: bad varint")
		}
		b = b[n:]
		switch tag {
		case tInt:
			return v, b, nil
		case tDate:
			return Date(v), b, nil
		case tTimestamp:
			return Timestamp(v), b, nil
		case tTimestampTZ:
			return TimestampTZ(v), b, nil
		default:
			return TimeOfDay(v), b, nil
		}
	case tFloat:
		if len(b) < 8 {
			return nil, nil, fmt.Errorf("doc: truncated float")
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), b[8:], nil
	case tString, tDecimal, tUUID:
		s, rest, err := readStr(b)
		if err != nil {
			return nil, nil, err
		}
		switch tag {
		case tString:
			return s, rest, nil
		case tDecimal:
			return Decimal(s), rest, nil
		default:
			return UUID(s), rest, nil
		}
	case tBytes:
		n, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < n {
			return nil, nil, fmt.Errorf("doc: truncated bytes")
		}
		out := append([]byte(nil), b[k:k+int(n)]...)
		return out, b[k+int(n):], nil
	case tObject:
		n, k := binary.Uvarint(b)
		if k <= 0 {
			return nil, nil, fmt.Errorf("doc: bad object length")
		}
		b = b[k:]
		o := make(Object, n)
		for i := uint64(0); i < n; i++ {
			key, rest, err := readStr(b)
			if err != nil {
				return nil, nil, err
			}
			v, rest2, err := decode(rest)
			if err != nil {
				return nil, nil, err
			}
			o[key] = v
			b = rest2
		}
		return o, b, nil
	case tList:
		n, k := binary.Uvarint(b)
		if k <= 0 {
			return nil, nil, fmt.Errorf("doc: bad list length")
		}
		b = b[k:]
		l := make(List, 0, n)
		for i := uint64(0); i < n; i++ {
			v, rest, err := decode(b)
			if err != nil {
				return nil, nil, err
			}
			l = append(l, v)
			b = rest
		}
		return l, b, nil
	default:
		return nil, nil, fmt.Errorf("doc: unknown tag %d", tag)
	}
}

func readStr(b []byte) (string, []byte, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 || uint64(len(b)-k) < n {
		return "", nil, fmt.Errorf("doc: truncated string")
	}
	return string(b[k : k+int(n)]), b[k+int(n):], nil
}
