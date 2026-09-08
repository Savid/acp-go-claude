// Package jsonvalue preserves exact numerical values at owned JSON boundaries.
package jsonvalue

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Int64 accepts any JSON spelling of an integer that fits int64. It bounds
// exponent work by the input length and never expands an untrusted exponent.
func Int64(number json.Number) (int64, bool) {
	text := string(number)
	if !json.Valid([]byte(text)) || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) {
		return 0, false
	}

	negative := strings.HasPrefix(text, "-")
	coefficient := strings.TrimPrefix(text, "-")

	exponentText := "0"
	if index := strings.IndexAny(coefficient, "eE"); index >= 0 {
		exponentText, coefficient = coefficient[index+1:], coefficient[:index]
	}

	fraction := 0
	if index := strings.IndexByte(coefficient, '.'); index >= 0 {
		fraction = len(coefficient) - index - 1
		coefficient = coefficient[:index] + coefficient[index+1:]
	}

	digits := strings.TrimLeft(coefficient, "0")
	if digits == "" {
		return 0, true
	}

	exponent, err := strconv.Atoi(exponentText)
	if err != nil || exponent > len(text)+19 || exponent < -len(text)-19 {
		return 0, false
	}

	trimmed := strings.TrimRight(digits, "0")

	scale := exponent - fraction + len(digits) - len(trimmed)
	if scale < 0 || len(trimmed)+scale > 19 {
		return 0, false
	}

	integer := trimmed + strings.Repeat("0", scale)
	if negative {
		integer = "-" + integer
	}

	value, err := strconv.ParseInt(integer, 10, 64)

	return value, err == nil
}
