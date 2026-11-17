package main

// A JSON encoder for the values the REST handlers build (maps, slices, strings, integers, booleans,
// nil), byte for byte what encoding/json writes for them: map keys sorted, HTML characters and
// U+2028/U+2029 escaped, invalid UTF-8 replaced by U+FFFD. encoding/json reaches the same output
// through reflection and a sort per map; this walks the concrete types directly. Anything else (a
// float, a struct, a json.Marshaler) goes to encoding/json for that value.

import (
	"encoding/json"
	"slices"
	"strconv"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

func appendJSON(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...), nil
	case string:
		return appendJSONString(dst, x), nil
	case bool:
		if x {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case int:
		return strconv.AppendInt(dst, int64(x), 10), nil
	case int64:
		return strconv.AppendInt(dst, x, 10), nil
	case int32:
		return strconv.AppendInt(dst, int64(x), 10), nil
	case uint64:
		return strconv.AppendUint(dst, x, 10), nil
	case map[string]any:
		if x == nil {
			return append(dst, "null"...), nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			var err error
			if dst, err = appendJSON(dst, x[k]); err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	case []any:
		if x == nil {
			return append(dst, "null"...), nil
		}
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			if dst, err = appendJSON(dst, e); err != nil {
				return dst, err
			}
		}
		return append(dst, ']'), nil
	case []map[string]any:
		if x == nil {
			return append(dst, "null"...), nil
		}
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			if dst, err = appendJSON(dst, e); err != nil {
				return dst, err
			}
		}
		return append(dst, ']'), nil
	case []string:
		if x == nil {
			return append(dst, "null"...), nil
		}
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, e)
		}
		return append(dst, ']'), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return dst, err
	}
	return append(dst, b...), nil
}

// appendJSONString writes s as encoding/json does with HTML escaping on.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, 0x5c, 'u', 'f', 'f', 'f', 'd') // the six characters �
			i += size
			start = i
			continue
		}
		if c == 0x2028 || c == 0x2029 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
