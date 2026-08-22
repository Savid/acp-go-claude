package lifecycle

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
)

// decodeValue reads one JSON document into the generic form every lifecycle
// comparison runs on. Numbers are retained as their literals rather than read
// through binary floating point: a consumer that certifies a boundary compares
// values losslessly, and a float64 both loses digits beyond its precision and
// refuses a literal beyond its range outright. A document with anything after
// its first value is not one value, and is not decodable here.
func decodeValue(raw []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}

	return value, !decoder.More()
}

// valueEqual reports lifecycle value equality. Two decoded values are equal when
// they are deeply equal member by member — key order and insignificant
// whitespace are never differences — and their numbers name the same exact
// mathematical value. This is deliberately stricter than a canonicalization that
// collapses numbers to doubles: two integers past double precision differing in
// one digit are two values, and a consumer that reads them as one cannot certify
// anything about the stream carrying them.
func valueEqual(left, right any) bool {
	switch left := left.(type) {
	case map[string]any:
		return objectEqual(left, right)
	case []any:
		return arrayEqual(left, right)
	case json.Number:
		other, ok := right.(json.Number)

		return ok && numberEqual(left, other)
	default:
		return left == right
	}
}

func objectEqual(left map[string]any, right any) bool {
	members, ok := right.(map[string]any)
	if !ok || len(left) != len(members) {
		return false
	}

	for key, value := range left {
		other, present := members[key]
		if !present || !valueEqual(value, other) {
			return false
		}
	}

	return true
}

func arrayEqual(left []any, right any) bool {
	entries, ok := right.([]any)
	if !ok || len(left) != len(entries) {
		return false
	}

	for index := range left {
		if !valueEqual(left[index], entries[index]) {
			return false
		}
	}

	return true
}

// numberEqual compares two JSON number literals as exact mathematical values.
// The comparison is non-expanding: each literal is read as the normalized
// decimal triple of a sign, a coefficient stripped of leading and trailing
// zeros, and the power of ten that coefficient is scaled by. A twelve-byte
// literal naming a number no machine can hold therefore costs its own length
// and never its expansion. Every zero is the same zero, whatever its sign or
// scale.
func numberEqual(left, right json.Number) bool {
	if left == right {
		return true
	}

	leftSign, leftDigits, leftScale := normalizedNumber(string(left))
	rightSign, rightDigits, rightScale := normalizedNumber(string(right))

	if leftDigits == "" || rightDigits == "" {
		return leftDigits == rightDigits
	}

	return leftSign == rightSign && leftDigits == rightDigits && leftScale.Cmp(rightScale) == 0
}

// normalizedNumber reads one JSON number literal into its normalized decimal
// triple. The exponent is arbitrary-precision because the literal's is: reading
// it into a machine integer would decide equality by how large an exponent the
// host happens to hold. A zero coefficient reports an empty digit string, which
// is what makes every spelling of zero one value.
func normalizedNumber(literal string) (negative bool, coefficient string, scale *big.Int) {
	scale = new(big.Int)

	if rest, cut := strings.CutPrefix(literal, "-"); cut {
		negative = true
		literal = rest
	}

	if marker := strings.IndexAny(literal, "eE"); marker >= 0 {
		scale.SetString(literal[marker+1:], 10)
		literal = literal[:marker]
	}

	integral, fraction, _ := strings.Cut(literal, ".")

	// The fraction digits join the coefficient, so the value they carry is
	// recovered by lowering the scale one place per digit.
	scale.Sub(scale, big.NewInt(int64(len(fraction))))

	digits := strings.TrimLeft(integral+fraction, "0")
	coefficient = strings.TrimRight(digits, "0")
	scale.Add(scale, big.NewInt(int64(len(digits)-len(coefficient))))

	return negative, coefficient, scale
}
