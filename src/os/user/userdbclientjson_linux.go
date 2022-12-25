//go:build linux

package user

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type userdbParamsUnmarshaler interface {
	unmarshalParameters([]jsonObject) error
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// findElementStart returns a slice of r that starts at the next JSON element.
// It skips over valid JSON space characters and checks for the colon separator.
func findElementStart(r []byte) ([]byte, error) {
	var idx int
	var b byte
	colon := byte(':')
	var seenColon bool

	for idx, b = range r {
		if isSpace(b) {
			continue
		}
		if !seenColon && b == colon {
			seenColon = true
			continue
		}
		// Spotted colon and b is not a space, so value starts here.
		if seenColon {
			break
		}
		return nil, errors.New("expected colon, got invalid character: " + string(b))
	}

	if !seenColon {
		return nil, errors.New("expected colon, got end of input")
	}
	return r[idx:], nil
}

func parseJSONElement(r []byte) (any, []byte, error) {
	for len(r) > 0 && isSpace(r[0]) {
		r = r[1:]
	}

	switch r[0] {
	case '{':
		return parseJSONObject(r)
	case '[':
		return parseJSONArray(r)
	case '"':
		return parseJSONString(r)
	case 't', 'f':
		return parseJSONBoolean(r)
	default:
		return parseJSONInt64(r)
	}
}

type jsonObject map[string]any

func jsonObjectGet[T any](o jsonObject, key string) (t T, ok bool) {
	a, ok := o[key]
	if !ok {
		return
	}

	t, ok = a.(T)
	return
}

// parseJSONObject reads a JSON object from r.
func parseJSONObject(r []byte) (jsonObject, []byte, error) {
	for len(r) > 0 && isSpace(r[0]) {
		r = r[1:]
	}

	if len(r) < 2 {
		return nil, r, errors.New("unexpected end of input")
	}

	if r[0] != '{' {
		return nil, r, errors.New("expected {")
	}

	r = r[1:]
	obj := make(jsonObject)
	for {
		if len(r) == 0 {
			return nil, r, errors.New("unexpected end of input")
		}
		if r[0] == '}' {
			return obj, r, nil
		}

		key, rest, err := parseJSONString(r)
		if err != nil {
			return nil, rest, err
		}
		r = rest

		r, err = findElementStart(r)
		if err != nil {
			return nil, r, err
		}

		value, rest, err := parseJSONElement(r)
		if err != nil {
			return nil, rest, err
		}
		obj[key] = value
		r = rest
	}
}

func parseJSONArray(r []byte) ([]any, []byte, error) {
	for len(r) > 0 && isSpace(r[0]) {
		r = r[1:]
	}

	if len(r) < 2 {
		return nil, r, errors.New("unexpected end of input")
	}

	if r[0] != '[' {
		return nil, r, errors.New("expected [")
	}

	r = r[1:]
	var elements []any
	for {
		if len(r) == 0 {
			return nil, r, errors.New("unexpected end of input")
		}
		if r[0] == ']' {
			return elements, r, nil
		}

		element, rest, err := parseJSONElement(r)
		if err != nil {
			return nil, rest, err
		}
		elements = append(elements, element)
		r = rest
	}
}

// parseJSONString reads a JSON string from r.
func parseJSONString(r []byte) (string, []byte, error) {
	for len(r) > 0 && isSpace(r[0]) {
		r = r[1:]
	}

	if len(r) < 2 {
		return "", r, errors.New("unexpected end of input")
	}

	// Smallest valid string is `""`.
	if r[0] == '"' && r[1] == '"' {
		return "", r[2:], nil
	}

	if c := r[0]; c != '"' {
		return "", r, errors.New(`expected " got ` + string(c))
	}

	// Advance over opening quote.
	r = r[1:]

	var value strings.Builder
	var inEsc bool
	var inUEsc bool
	var strEnds bool
	reader := bytes.NewReader(r)
	for {
		if value.Len() > 4096 {
			return "", r, errors.New("string too large")
		}

		// Parse unicode escape sequences.
		if inUEsc {
			maybeRune := make([]byte, 4)
			n, err := reader.Read(maybeRune)
			if err != nil || n != 4 {
				return "", r, fmt.Errorf("invalid unicode escape sequence \\u%s", string(maybeRune))
			}

			prn, err := strconv.ParseUint(string(maybeRune), 16, 32)
			if err != nil {
				return "", r, fmt.Errorf("invalid unicode escape sequence \\u%s", string(maybeRune))
			}
			rn := rune(prn)
			if !utf16.IsSurrogate(rn) {
				value.WriteRune(rn)
				inUEsc = false
				continue
			}

			// rn maybe a high surrogate; read the low surrogate.
			maybeRune = make([]byte, 6)
			n, err = reader.Read(maybeRune)
			if err != nil || n != 6 || maybeRune[0] != '\\' || maybeRune[1] != 'u' {
				// Not a valid UTF-16 surrogate pair.
				if _, err := reader.Seek(int64(-n), io.SeekCurrent); err != nil {
					return "", r, err
				}
				// Invalid low surrogate; write the replacement character.
				value.WriteRune(utf8.RuneError)
			} else {
				rn1, err := strconv.ParseUint(string(maybeRune[2:]), 16, 32)
				if err != nil {
					return "", r, fmt.Errorf("invalid unicode escape sequence %s", string(maybeRune))
				}
				// Check if rn and rn1 are valid UTF-16 surrogate pairs.
				if dec := utf16.DecodeRune(rn, rune(rn1)); dec != utf8.RuneError {
					n = utf8.EncodeRune(maybeRune, dec)
					// Write the decoded rune.
					value.Write(maybeRune[:n])
				}
			}
			inUEsc = false
			continue
		}

		if inEsc {
			b, err := reader.ReadByte()
			if err != nil {
				return "", r, err
			}
			switch b {
			case 'b':
				value.WriteByte('\b')
			case 'f':
				value.WriteByte('\f')
			case 'n':
				value.WriteByte('\n')
			case 'r':
				value.WriteByte('\r')
			case 't':
				value.WriteByte('\t')
			case 'u':
				inUEsc = true
			case '/':
				value.WriteByte('/')
			case '\\':
				value.WriteByte('\\')
			case '"':
				value.WriteByte('"')
			default:
				return "", r, errors.New("unexpected character in escape sequence " + string(b))
			}
			inEsc = false
			continue
		} else {
			rn, _, err := reader.ReadRune()
			if err != nil {
				if err == io.EOF {
					break
				}
				return "", r, err
			}
			if rn == '\\' {
				inEsc = true
				continue
			}
			if rn == '"' {
				// String ends on un-escaped quote.
				strEnds = true
				break
			}
			value.WriteRune(rn)
		}
	}

	if !strEnds {
		return "", r, errors.New("unexpected end of input")
	}

	return value.String(), r[reader.Len():], nil
}

// parseJSONInt64 reads a 64 bit integer from r.
func parseJSONInt64(r []byte) (int64, []byte, error) {
	for len(r) > 0 && isSpace(r[0]) {
		r = r[1:]
	}

	var num strings.Builder
	var count int
	for _, b := range r {
		// int64 max is 19 digits long.
		if num.Len() == 20 {
			return 0, r, errors.New("number too large")
		}
		if strings.ContainsRune("0123456789", rune(b)) {
			num.WriteByte(b)
		} else {
			break
		}

		count++
	}

	n, err := strconv.ParseInt(num.String(), 10, 64)
	return int64(n), r[count:], err
}

var (
	bytesTrue  = []byte("true")
	bytesFalse = []byte("false")
)

// parseJSONBoolean reads a boolean from r.
func parseJSONBoolean(r []byte) (bool, []byte, error) {
	for len(r) > 0 && isSpace(r[0]) {
		r = r[1:]
	}

	if len(r) >= 4 && bytes.Equal(r[:4], bytesTrue) {
		return true, r[4:], nil
	}
	if len(r) >= 5 && bytes.Equal(r[:5], bytesFalse) {
		return false, r[5:], nil
	}

	return false, r, errors.New("unable to parse boolean value")
}
