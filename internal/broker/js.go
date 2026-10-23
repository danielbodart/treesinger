package broker

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// The TypeScript original reads upstream JSON and the environment with
// JavaScript's own loose rules: `body?.error` looks up exactly that key and
// takes whatever is there, `Number(text)` reads hex and exponents, `x * 1000`
// coerces a numeric string. encoding/json and strconv are stricter, and
// encoding/json matches struct fields regardless of case. The helpers here
// read the same inputs the way JavaScript does, so the two brokers accept and
// answer the same things.

// jsonObject is data as a JSON object with its keys exactly as sent, or false
// when it is not one (an array, a scalar, null, or not JSON at all). Like
// JSON.parse, a repeated key keeps its last value.
func jsonObject(data []byte) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

// present is whether a looked-up value is there at all: neither missing nor
// null, the two that `??` passes over.
func present(raw json.RawMessage) bool {
	return raw != nil && string(raw) != "null"
}

// jsonString is raw when it is a JSON string.
func jsonString(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

// jsonText is raw as a string field the way JavaScript would use it in a
// template literal: a string as is, any other value as String(value), and a
// missing or null one as "".
func jsonText(raw json.RawMessage) string {
	if !present(raw) {
		return ""
	}
	if text, isString := jsonString(raw); isString {
		return text
	}
	return jsString(raw)
}

// jsString is String(value) of a JSON value: what `${value}` shows.
func jsString(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	return stringOf(value)
}

func stringOf(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return numberString(v)
	case []any:
		// Array.prototype.join: null and undefined elements are empty.
		parts := make([]string, len(v))
		for i, element := range v {
			if element != nil {
				parts[i] = stringOf(element)
			}
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// numberString is Number.prototype.toString for the values JSON can carry.
func numberString(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	if abs := math.Abs(f); abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	sign := exponent[0]
	return mantissa + "e" + string(sign) + strings.TrimLeft(exponent[1:], "0")
}

// jsonNumber is raw coerced as `raw * 1000` would coerce it: a number as is,
// a numeric string read by Number(), null and false as 0, true as 1, and
// anything else (missing included) as NaN.
func jsonNumber(raw json.RawMessage) float64 {
	if raw == nil {
		return math.NaN()
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return math.NaN()
	}
	switch v := value.(type) {
	case nil:
		return 0
	case bool:
		if v {
			return 1
		}
		return 0
	case float64:
		return v
	case string:
		return jsNumber(v)
	case []any:
		// Number([]) is 0 and Number([x]) is Number(String(x)).
		if len(v) <= 1 {
			return jsNumber(stringOf(v))
		}
	}
	return math.NaN()
}

var decimalLiteral = regexp.MustCompile(`^[+-]?(Infinity|([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?)$`)

// jsNumber is JavaScript's Number(text): surrounding whitespace ignored, an
// empty string is 0, decimal with an optional exponent, Infinity, or an
// unsigned 0x/0o/0b integer; anything else is NaN.
func jsNumber(text string) float64 {
	text = jsTrim(text)
	if text == "" {
		return 0
	}
	if len(text) > 2 && text[0] == '0' {
		base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[text[1]]
		if base != 0 {
			if strings.ContainsAny(text[2:], "_+-") {
				return math.NaN()
			}
			n, err := strconv.ParseUint(text[2:], base, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(n)
		}
	}
	if !decimalLiteral.MatchString(text) {
		return math.NaN()
	}
	switch strings.TrimLeft(text, "+-") {
	case "Infinity":
		if text[0] == '-' {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	// Out of range comes back as ±Inf, as it does in JavaScript.
	f, _ := strconv.ParseFloat(text, 64)
	return f
}

// jsTrim is String.prototype.trim: JavaScript's whitespace and line
// terminators, which include the byte-order mark U+FEFF but not U+0085.
func jsTrim(text string) string {
	return strings.TrimFunc(text, isJSSpace)
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

// isFalsy is whether a JSON value is one JavaScript treats as false.
func isFalsy(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", "false", `""`:
		return true
	}
	var number float64
	return json.Unmarshal(raw, &number) == nil && number == 0
}
