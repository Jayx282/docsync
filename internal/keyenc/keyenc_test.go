package keyenc

import (
	"bytes"
	"math"
	"reflect"
	"sort"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	cases := [][]any{
		{int64(0)}, {int64(-1)}, {int64(math.MaxInt64)}, {int64(math.MinInt64)},
		{"", "a", "a\x00b"}, {[]byte{0, 0xFF, 1}}, {nil, true, false}, {int64(5), "x", nil},
	}
	for _, c := range cases {
		b, err := Encode(c...)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normalize(got), normalize(c)) {
			t.Fatalf("round trip %v -> %v", c, got)
		}
	}
}

func normalize(xs []any) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		if b, ok := x.([]byte); ok {
			out[i] = string(b) + "\x00bytes"
		} else {
			out[i] = x
		}
	}
	return out
}

func TestIntOrder(t *testing.T) {
	vals := []int64{math.MinInt64, -1000, -1, 0, 1, 2, 1000, math.MaxInt64}
	var encs [][]byte
	for _, v := range vals {
		encs = append(encs, MustEncode(v))
	}
	if !sort.SliceIsSorted(encs, func(i, j int) bool { return bytes.Compare(encs[i], encs[j]) < 0 }) {
		t.Fatal("encoded ints are not ordered")
	}
}

func TestStringPrefixOrder(t *testing.T) {
	a, b := MustEncode("ab", int64(1)), MustEncode("ab\x00", int64(0))
	if bytes.Compare(a, b) >= 0 {
		t.Fatal("tuple (\"ab\",1) must sort before (\"ab\\x00\",0)")
	}
}

func TestPrefixEnd(t *testing.T) {
	if got := PrefixEnd([]byte("d/")); string(got) != "d0" {
		t.Fatalf("PrefixEnd(d/) = %q", got)
	}
	if PrefixEnd([]byte{0xFF, 0xFF}) != nil {
		t.Fatal("all-0xFF prefix has no end")
	}
}
