package doc

import (
	"bytes"
	"math"
	"reflect"
	"testing"
	"time"
)

func sample() Object {
	return Object{
		"id":     int64(42),
		"carat":  Decimal("1.250"),
		"price":  1234.5,
		"name":   "Ébène \"quoted\" <tag>\n",
		"guid":   UUID("6F9619FF-8B86-D011-B42D-00C04FC964FF"),
		"day":    DateOf(time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC)),
		"at":     TimestampOf(time.Date(2026, 11, 7, 15, 4, 5, 123456000, time.UTC)),
		"atz":    TimestampTZOf(time.Date(2026, 11, 7, 20, 34, 5, 0, time.FixedZone("IST", 5*3600+1800))),
		"clock":  TimeOfDayOf(time.Date(1, 1, 1, 9, 30, 0, 500000000, time.UTC)),
		"bin":    []byte{1, 2, 3},
		"ok":     true,
		"none":   nil,
		"list":   List{Object{"k": int64(1)}, Object{"k": int64(2)}},
		"nested": Object{"b": "x", "a": int64(-7)},
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	o := sample()
	b, err := Encode(o)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeObject(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, o) {
		t.Fatalf("round trip mismatch:\n%#v\n%#v", got, o)
	}
}

func TestEncodingIsCanonical(t *testing.T) {
	a := Object{"x": int64(1), "y": "z", "n": Object{"q": int64(2), "p": int64(3)}}
	b := Object{"n": Object{"p": int64(3), "q": int64(2)}, "y": "z", "x": int64(1)}
	if !bytes.Equal(MustEncode(a), MustEncode(b)) {
		t.Fatal("equal objects must encode identically regardless of map order")
	}
}

func TestJSON(t *testing.T) {
	got := MustJSON(sample())
	want := `{"at":"2026-11-07T15:04:05.123456","atz":"2026-11-07T15:04:05Z","bin":"AQID","carat":1.250,"clock":"09:30:00.5","day":"2026-11-07","guid":"6F9619FF-8B86-D011-B42D-00C04FC964FF","id":42,"list":[{"k":1},{"k":2}],"name":"Ébène \"quoted\" <tag>\n","nested":{"a":-7,"b":"x"},"none":null,"ok":true,"price":1234.5}`
	if got != want {
		t.Fatalf("JSON:\n got %s\nwant %s", got, want)
	}
	if MustJSON(math.Inf(1)) != `"Infinity"` {
		t.Fatal("infinity must render as a string")
	}
}

func TestNormalizeDecimal(t *testing.T) {
	for in, want := range map[string]Decimal{"1.2": "1.200", "-0.5": "-0.500", "12": "12.000", "1.2345": "1.235"} {
		got, err := NormalizeDecimal(in, 3)
		if err != nil || got != want {
			t.Fatalf("NormalizeDecimal(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestCompare(t *testing.T) {
	ordered := []any{nil, false, true, int64(-3), Decimal("-2.5"), 1.0, int64(2), Decimal("2.5"), "a", "b", UUID("A")}
	for i := 0; i < len(ordered)-1; i++ {
		if Compare(ordered[i], ordered[i+1]) >= 0 {
			t.Fatalf("Compare(%v, %v) should be < 0", ordered[i], ordered[i+1])
		}
	}
	if Compare(int64(2), Decimal("2.000")) != 0 {
		t.Fatal("2 and 2.000 compare equal numerically")
	}
}
