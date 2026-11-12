// Package keyenc encodes tuples of primitive values into byte strings whose
// lexicographic order matches the tuple order. It is used for state-store keys.
//
// Supported element types: nil, bool, int64 (and smaller ints), string, []byte.
package keyenc

import (
	"encoding/binary"
	"fmt"
)

const (
	tagNil    = 0x00
	tagBytes  = 0x01
	tagString = 0x02
	tagInt    = 0x15
	tagFalse  = 0x26
	tagTrue   = 0x27

	escape = 0xFF
)

// Encode encodes the values as one tuple.
func Encode(vals ...any) ([]byte, error) {
	var out []byte
	var err error
	for _, v := range vals {
		out, err = Append(out, v)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MustEncode is Encode for values known to be supported.
func MustEncode(vals ...any) []byte {
	b, err := Encode(vals...)
	if err != nil {
		panic(err)
	}
	return b
}

// Append appends one encoded element to dst.
func Append(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, tagNil), nil
	case bool:
		if x {
			return append(dst, tagTrue), nil
		}
		return append(dst, tagFalse), nil
	case int64:
		return appendInt(dst, x), nil
	case int:
		return appendInt(dst, int64(x)), nil
	case int32:
		return appendInt(dst, int64(x)), nil
	case int16:
		return appendInt(dst, int64(x)), nil
	case int8:
		return appendInt(dst, int64(x)), nil
	case uint8:
		return appendInt(dst, int64(x)), nil
	case string:
		return appendEscaped(append(dst, tagString), []byte(x)), nil
	case []byte:
		return appendEscaped(append(dst, tagBytes), x), nil
	default:
		return nil, fmt.Errorf("keyenc: unsupported type %T", v)
	}
}

func appendInt(dst []byte, v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v)^(1<<63))
	return append(append(dst, tagInt), b[:]...)
}

// appendEscaped writes b with 0x00 escaped as 0x00 0xFF and a 0x00 terminator,
// which keeps prefix ordering intact.
func appendEscaped(dst, b []byte) []byte {
	for _, c := range b {
		dst = append(dst, c)
		if c == 0x00 {
			dst = append(dst, escape)
		}
	}
	return append(dst, 0x00)
}

// Decode decodes a tuple produced by Encode.
func Decode(b []byte) ([]any, error) {
	var out []any
	for len(b) > 0 {
		v, rest, err := decodeOne(b)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		b = rest
	}
	return out, nil
}

func decodeOne(b []byte) (any, []byte, error) {
	switch b[0] {
	case tagNil:
		return nil, b[1:], nil
	case tagFalse:
		return false, b[1:], nil
	case tagTrue:
		return true, b[1:], nil
	case tagInt:
		if len(b) < 9 {
			return nil, nil, fmt.Errorf("keyenc: truncated int")
		}
		u := binary.BigEndian.Uint64(b[1:9]) ^ (1 << 63)
		return int64(u), b[9:], nil
	case tagString, tagBytes:
		raw, rest, err := readEscaped(b[1:])
		if err != nil {
			return nil, nil, err
		}
		if b[0] == tagString {
			return string(raw), rest, nil
		}
		return raw, rest, nil
	default:
		return nil, nil, fmt.Errorf("keyenc: unknown tag 0x%02x", b[0])
	}
}

func readEscaped(b []byte) ([]byte, []byte, error) {
	var out []byte
	for i := 0; i < len(b); i++ {
		if b[i] != 0x00 {
			out = append(out, b[i])
			continue
		}
		if i+1 < len(b) && b[i+1] == escape {
			out = append(out, 0x00)
			i++
			continue
		}
		return out, b[i+1:], nil
	}
	return nil, nil, fmt.Errorf("keyenc: unterminated string")
}

// PrefixEnd returns the smallest key greater than every key with the given prefix,
// or nil when no such key exists. Useful as an exclusive iterator upper bound.
func PrefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xFF {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}
