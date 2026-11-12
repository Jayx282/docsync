package doc

import (
	"bytes"
	"math/big"
	"strings"
)

// rank orders values of different types: null first, then booleans, numbers,
// strings, temporal values and binary. Lists and objects sort last by encoding.
func rank(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case bool:
		return 1
	case int64, float64, Decimal:
		return 2
	case string:
		return 3
	case UUID:
		return 4
	case Date:
		return 5
	case Timestamp:
		return 6
	case TimestampTZ:
		return 7
	case TimeOfDay:
		return 8
	case []byte:
		return 9
	default:
		return 10
	}
}

// Compare orders two values deterministically. Numbers compare numerically across
// int, float and decimal; strings compare by bytes (not by SQL collation).
func Compare(a, b any) int {
	ra, rb := rank(a), rank(b)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch x := a.(type) {
	case nil:
		return 0
	case bool:
		y := b.(bool)
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		default:
			return 1
		}
	case int64, float64, Decimal:
		return compareNumbers(a, b)
	case string:
		return strings.Compare(x, b.(string))
	case UUID:
		return strings.Compare(string(x), string(b.(UUID)))
	case Date:
		return cmpInt(int64(x), int64(b.(Date)))
	case Timestamp:
		return cmpInt(int64(x), int64(b.(Timestamp)))
	case TimestampTZ:
		return cmpInt(int64(x), int64(b.(TimestampTZ)))
	case TimeOfDay:
		return cmpInt(int64(x), int64(b.(TimeOfDay)))
	case []byte:
		return bytes.Compare(x, b.([]byte))
	default:
		return bytes.Compare(MustEncode(a), MustEncode(b))
	}
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareNumbers(a, b any) int {
	if x, ok := a.(int64); ok {
		if y, ok := b.(int64); ok {
			return cmpInt(x, y)
		}
	}
	ra, rb := toRat(a), toRat(b)
	if ra == nil || rb == nil { // NaN or infinity: fall back to encodings for determinism
		return bytes.Compare(MustEncode(a), MustEncode(b))
	}
	return ra.Cmp(rb)
}

func toRat(v any) *big.Rat {
	switch x := v.(type) {
	case int64:
		return new(big.Rat).SetInt64(x)
	case float64:
		r := new(big.Rat)
		if r.SetFloat64(x) == nil {
			return nil
		}
		return r
	case Decimal:
		r, ok := new(big.Rat).SetString(string(x))
		if !ok {
			return nil
		}
		return r
	}
	return nil
}
