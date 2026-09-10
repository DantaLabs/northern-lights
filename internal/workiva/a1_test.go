package workiva

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestA1ToRange(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Range
	}{
		{"rect", "B3:D10", Range{StartRow: 2, StartCol: 1, StopRow: 9, StopCol: 3}},
		{"originCell", "A1", Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 0}},
		{"singleCell", "B3", Range{StartRow: 2, StartCol: 1, StopRow: 2, StopCol: 1}},
		{"openColumns", "A:C", Range{StartRow: -1, StartCol: 0, StopRow: -1, StopCol: 2}},
		{"openRows", "3:10", Range{StartRow: 2, StartCol: -1, StopRow: 9, StopCol: -1}},
		{"wholeColumn", "A:A", Range{StartRow: -1, StartCol: 0, StopRow: -1, StopCol: 0}},
		{"beyondZSingle", "AA1", Range{StartRow: 0, StartCol: 26, StopRow: 0, StopCol: 26}},
		{"beyondZRect", "AA1:AB2", Range{StartRow: 0, StartCol: 26, StopRow: 1, StopCol: 27}},
		{"tripleLetterColumn", "AAA1", Range{StartRow: 0, StartCol: 702, StopRow: 0, StopCol: 702}},
		{"lowercaseNormalized", "b3:d4", Range{StartRow: 2, StartCol: 1, StopRow: 3, StopCol: 3}},
		{"whitespaceTrimmed", " B3:D10 ", Range{StartRow: 2, StartCol: 1, StopRow: 9, StopCol: 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := A1ToRange(tc.in)
			if err != nil {
				t.Fatalf("A1ToRange(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("A1ToRange(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestA1ToRangeInvalid(t *testing.T) {
	for _, in := range []string{
		"",
		"A0",
		"0",
		"A1:B2:C3",
		"A1:",
		":B2",
		"1A",
		"A 1",
		"@1",
		"A1:C",
		"A:3",
		"3:A",
		"::",
		"ZZZZZZZZZZZZZZ1",
		"FXSHRXY1",
		"A2147483649",
	} {
		if _, err := A1ToRange(in); err == nil {
			t.Errorf("A1ToRange(%q) returned nil error, want error", in)
		}
	}
}

func TestA1ToRangeAcceptsMaximumInt32Coordinate(t *testing.T) {
	got, err := A1ToRange("FXSHRXX2147483648")
	if err != nil {
		t.Fatalf("A1ToRange maximum coordinate: %v", err)
	}
	want := Range{StartRow: 2147483647, StartCol: 2147483647, StopRow: 2147483647, StopCol: 2147483647}
	if got != want {
		t.Errorf("maximum coordinate = %+v, want %+v", got, want)
	}
	back, err := RangeToA1(got)
	if err != nil {
		t.Fatalf("RangeToA1 maximum coordinate: %v", err)
	}
	if back != "FXSHRXX2147483648" {
		t.Errorf("maximum coordinate round trip = %q", back)
	}
}

func TestRangeToA1(t *testing.T) {
	tests := []struct {
		name string
		in   Range
		want string
	}{
		{"rect", Range{StartRow: 2, StartCol: 1, StopRow: 9, StopCol: 3}, "B3:D10"},
		{"originCell", Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 0}, "A1"},
		{"singleCell", Range{StartRow: 2, StartCol: 1, StopRow: 2, StopCol: 1}, "B3"},
		{"openColumns", Range{StartRow: -1, StartCol: 0, StopRow: -1, StopCol: 2}, "A:C"},
		{"openRows", Range{StartRow: 2, StartCol: -1, StopRow: 9, StopCol: -1}, "3:10"},
		{"wholeColumn", Range{StartRow: -1, StartCol: 0, StopRow: -1, StopCol: 0}, "A:A"},
		{"beyondZRect", Range{StartRow: 0, StartCol: 26, StopRow: 1, StopCol: 27}, "AA1:AB2"},
		{"singleRow", Range{StartRow: 4, StartCol: 0, StopRow: 4, StopCol: 2}, "A5:C5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RangeToA1(tc.in)
			if err != nil {
				t.Fatalf("RangeToA1(%+v) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("RangeToA1(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRangeToA1Invalid(t *testing.T) {
	tests := []struct {
		name string
		in   Range
	}{
		{"fullyUnbounded", Range{StartRow: -1, StartCol: -1, StopRow: -1, StopCol: -1}},
		{"mixedRowBounds", Range{StartRow: -1, StartCol: 0, StopRow: 3, StopCol: 2}},
		{"mixedColBounds", Range{StartRow: 0, StartCol: -1, StopRow: 3, StopCol: 2}},
		{"rowStartAfterStop", Range{StartRow: 5, StartCol: 0, StopRow: 2, StopCol: 2}},
		{"colStartAfterStop", Range{StartRow: 0, StartCol: 5, StopRow: 2, StopCol: 2}},
		{"negativeBelowMinusOne", Range{StartRow: -2, StartCol: 0, StopRow: 2, StopCol: 2}},
	}
	if strconv.IntSize > 32 {
		aboveInt32 := int64(2147483648)
		tests = append(tests,
			struct {
				name string
				in   Range
			}{"rowAboveInt32", Range{StartRow: 0, StartCol: 0, StopRow: int(aboveInt32), StopCol: 0}},
			struct {
				name string
				in   Range
			}{"columnAboveInt32", Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: int(aboveInt32)}},
		)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RangeToA1(tc.in); err == nil {
				t.Errorf("RangeToA1(%+v) returned nil error, want error", tc.in)
			}
		})
	}
}

func TestA1RoundTrip(t *testing.T) {
	for _, a1 := range []string{"B3:D10", "A1", "B3", "A:C", "3:10", "A:A", "AA1:AB2"} {
		r, err := A1ToRange(a1)
		if err != nil {
			t.Fatalf("A1ToRange(%q): %v", a1, err)
		}
		back, err := RangeToA1(r)
		if err != nil {
			t.Fatalf("RangeToA1(%+v): %v", r, err)
		}
		if back != a1 {
			t.Errorf("round trip of %q gave %q", a1, back)
		}
	}
}

func TestRangeUnmarshalMapsNullAndMissingBoundsToUnboundedSentinel(t *testing.T) {
	var got Range
	if err := json.Unmarshal([]byte(`{"startColumn":0,"startRow":null,"stopColumn":1}`), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	want := Range{StartRow: -1, StartCol: 0, StopRow: -1, StopCol: 1}
	if got != want {
		t.Errorf("Range = %+v, want %+v", got, want)
	}
}

func TestRangeUnmarshalPreservesBoundedZero(t *testing.T) {
	var got Range
	if err := json.Unmarshal([]byte(`{"startColumn":0,"startRow":0,"stopColumn":0,"stopRow":0}`), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	want := Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 0}
	if got != want {
		t.Errorf("Range = %+v, want %+v", got, want)
	}
}

func TestRangeUnmarshalRejectsCoordinateAboveInt32(t *testing.T) {
	for _, raw := range []string{
		`{"startColumn":0,"startRow":0,"stopColumn":2147483648,"stopRow":0}`,
		`{"startColumn":0,"startRow":0,"stopColumn":0,"stopRow":2147483648}`,
	} {
		var got Range
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Errorf("json.Unmarshal(%s) returned nil error, want coordinate range error", raw)
		}
	}
}

func TestRangeMarshalUsesNullForUnboundedBounds(t *testing.T) {
	got, err := json.Marshal(Range{StartRow: -1, StartCol: 0, StopRow: -1, StopCol: 1})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	want := `{"startRow":null,"startColumn":0,"stopRow":null,"stopColumn":1}`
	if string(got) != want {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}
