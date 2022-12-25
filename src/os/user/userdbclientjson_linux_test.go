//go:build linux

package user

import (
	"bytes"
	"reflect"
	"strconv"
	"testing"
	"unicode/utf8"
)

var findElementStartTestCases = []struct {
	in   []byte
	want []byte
	err  bool
}{
	{in: []byte(`:`), want: []byte(``)},
	{in: []byte(`: `), want: []byte(``)},
	{in: []byte(`:"foo"`), want: []byte(`"foo"`)},
	{in: []byte(`  :"foo"`), want: []byte(`"foo"`)},
	{in: []byte(` 1231 :"foo"`), err: true},
	{in: []byte(``), err: true},
	{in: []byte(`"foo"`), err: true},
	{in: []byte(`foo`), err: true},
}

func TestFindElementStart(t *testing.T) {
	for i, tc := range findElementStartTestCases {
		t.Run("#"+strconv.Itoa(i), func(t *testing.T) {
			got, err := findElementStart(tc.in)
			if tc.err && err == nil {
				t.Errorf("want err for findElementStart(%s), got nil", tc.in)
			}
			if !tc.err {
				if err != nil {
					t.Errorf("findElementStart(%s) unexpected error: %s", tc.in, err.Error())
				}
				if !bytes.Contains(tc.in, got) {
					t.Errorf("%s should contain %s but does not", tc.in, got)
				}
			}
		})
	}
}

func FuzzFindElementStart(f *testing.F) {
	for _, tc := range findElementStartTestCases {
		if !tc.err {
			f.Add(tc.in)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if out, err := findElementStart(b); err == nil && !bytes.Contains(b, out) {
			t.Errorf("%s, %v", out, err)
		}
	})
}

var parseJSONObjectTestCases = []struct {
	in   []byte
	want jsonObject
	rest []byte
	err  bool
}{
	{in: []byte(`{"foo": "bar"}`), want: jsonObject{"foo": "bar"}},
	{in: []byte(`{"test": [1, 2, 3]} `), want: jsonObject{"test": []any{1, 2, 3}}, rest: []byte(` `)},
	{in: []byte(``), err: true},
	{in: []byte(`{}`), want: jsonObject{}},
	{in: []byte(`foo`), err: true},
	{in: []byte(`{"foo": "bar"`), err: true},
}

func TestParseJSONObject(t *testing.T) {
	for i, tc := range parseJSONObjectTestCases {
		t.Run("#"+strconv.Itoa(i), func(t *testing.T) {
			got, rest, err := parseJSONObject(tc.in)
			if tc.err && err == nil {
				t.Errorf("want err for parseJSONObject(%s), got nil", tc.in)
			}
			if !tc.err {
				if err != nil {
					t.Errorf("parseJSONObject(%s) unexpected error: %s", tc.in, err.Error())
				}
				if !reflect.DeepEqual(tc.want, got) {
					t.Errorf("parseJSONObject(%s) = %v, want %v", tc.in, got, tc.want)
				}
				if !bytes.Equal(tc.rest, rest) {
					t.Errorf("parseJSONObject(%s) rest = %s, want %s", tc.in, rest, tc.rest)
				}
			}
		})
	}
}

func FuzzParseJSONObject(f *testing.F) {
	for _, tc := range parseJSONObjectTestCases {
		f.Add(tc.in)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = parseJSONObject(b)
	})
}

var parseJSONStringTestCases = []struct {
	in   []byte
	want string
	err  bool
}{
	{in: []byte(`""`)},
	{in: []byte(`"\n"`), want: "\n"},
	{in: []byte(` "\""`), want: "\""},
	{in: []byte(`"\t \\"`), want: "\t \\"},
	{in: []byte(`"\\\\"`), want: `\\`},
	{in: []byte(`""`), err: true},
	{in: []byte(`"`), err: true},
	{in: []byte("\"0\xE5"), err: true},
	{in: []byte{'"', 0xFE, 0xFE, 0xFF, 0xFF, '"'}, want: "\uFFFD\uFFFD\uFFFD\uFFFD"},
	{in: []byte(`"\u0061a"`), want: "aa"},
	{in: []byte(`"\u0159\u0170"`), want: "řŰ"},
	{in: []byte(`"\uD800\uDC00"`), want: "\U00010000"},
	{in: []byte(`"\uD800"`), want: "\uFFFD"},
	{in: []byte(`"\u000"`), err: true},
	{in: []byte(`"\u00MF"`), err: true},
	{in: []byte(`"\uD800\uDC0"`), err: true},
}

func TestParseJSONString(t *testing.T) {
	for i, tc := range parseJSONStringTestCases {
		t.Run("#"+strconv.Itoa(i), func(t *testing.T) {
			got, _, err := parseJSONString(tc.in)
			if tc.err && err == nil {
				t.Errorf("want err for parseJSONString(%s), got nil", tc.in)
			}
			if !tc.err {
				if err != nil {
					t.Errorf("parseJSONString(%s) unexpected error: %s", tc.in, err.Error())
				}
				if tc.want != got {
					t.Errorf("parseJSONString(%s) = %s, want %s", tc.in, got, tc.want)
				}
			}
		})
	}
}

func FuzzParseJSONString(f *testing.F) {
	for _, tc := range parseJSONStringTestCases {
		f.Add(tc.in)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if out, _, err := parseJSONString(b); err == nil && !utf8.ValidString(out) {
			t.Errorf("parseJSONString(%s) = %s, invalid string", b, out)
		}
	})
}

var parseJSONInt64TestCases = []struct {
	in   []byte
	want int64
	rest []byte
	err  bool
}{
	{in: []byte("1235  "), want: 1235, rest: []byte("  ")},
	{in: []byte(" 123"), want: 123},
	{in: []byte("0")},
	{in: []byte("5012313123131231"), want: 5012313123131231},
	{in: []byte("231"), err: true},
}

func TestParseJSONInt64(t *testing.T) {
	for i, tc := range parseJSONInt64TestCases {
		t.Run("#"+strconv.Itoa(i), func(t *testing.T) {
			got, rest, err := parseJSONInt64(tc.in)
			if tc.err && err == nil {
				t.Errorf("want err for parseJSONInt64(%s), got nil", tc.in)
			}
			if !tc.err {
				if err != nil {
					t.Errorf("parseJSONInt64(%s) unexpected error: %s", tc.in, err.Error())
				}
				if tc.want != got {
					t.Errorf("parseJSONInt64(%s) = %d, want %d", tc.in, got, tc.want)
				}
				if !bytes.Equal(tc.rest, rest) {
					t.Errorf("parseJSONInt64(%s) rest = %s, want %s", tc.in, rest, tc.rest)
				}
			}
		})
	}
}

func FuzzParseJSONInt64(f *testing.F) {
	for _, tc := range parseJSONInt64TestCases {
		f.Add(tc.in)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if out, _, err := parseJSONInt64(b); err == nil &&
			!bytes.Contains(b, []byte(strconv.FormatInt(out, 10))) {
			t.Errorf("parseJSONInt64(%s) = %d, %v", b, out, err)
		}
	})
}

var parseJSONBooleanTestCases = []struct {
	in   []byte
	want bool
	rest []byte
	err  bool
}{
	{in: []byte(" true "), rest: []byte(" "), want: true},
	{in: []byte("true  "), rest: []byte("  "), want: true},
	{in: []byte(" false  "), rest: []byte("  "), want: false},
	{in: []byte("false  "), rest: []byte("  "), want: false},
	{in: []byte("foo"), err: true},
}

func TestParseJSONBoolean(t *testing.T) {
	for i, tc := range parseJSONBooleanTestCases {
		t.Run("#"+strconv.Itoa(i), func(t *testing.T) {
			got, rest, err := parseJSONBoolean(tc.in)
			if tc.err && err == nil {
				t.Errorf("want err for parseJSONBoolean(%s), got nil", tc.in)
			}
			if !tc.err {
				if err != nil {
					t.Errorf("parseJSONBoolean(%s) unexpected error: %s", tc.in, err.Error())
				}
				if tc.want != got {
					t.Errorf("parseJSONBoolean(%s) = %t, want %t", tc.in, got, tc.want)
				}
				if !bytes.Equal(tc.rest, rest) {
					t.Errorf("parseJSONBoolean(%s) rest = %v, want %v", tc.in, rest, tc.rest)
				}
			}
		})
	}
}

func FuzzParseJSONBoolean(f *testing.F) {
	for _, tc := range parseJSONBooleanTestCases {
		f.Add(tc.in)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if out, _, err := parseJSONBoolean(b); err == nil && !bytes.Contains(b, []byte(strconv.FormatBool(out))) {
			t.Errorf("parseJSONBoolean(%s) = %t, %v", b, out, err)
		}
	})
}
