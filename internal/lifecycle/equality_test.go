package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNumbersCompareAsExactMathematicalValues pins the number half of lifecycle
// value equality. The spelling a number arrived in is not content, so scale and
// trailing zeros are no difference; the value is, so two integers past double
// precision differing in one digit are two values. A reducer that compared
// lexemes would refuse the first class and one that decoded through a float
// would merge the second.
func TestNumbersCompareAsExactMathematicalValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		left  string
		right string
		equal bool
	}{
		{name: "identical lexemes", left: "7", right: "7", equal: true},
		{name: "trailing fraction zero", left: "1", right: "1.0", equal: true},
		{name: "scaled coefficient", left: "1e2", right: "100", equal: true},
		{name: "explicit positive exponent", left: "1e+2", right: "1E2", equal: true},
		{name: "negative scale", left: "0.001", right: "1e-3", equal: true},
		{name: "every zero is one zero", left: "-0", right: "0.000", equal: true},
		{name: "zero at any scale", left: "0e999999999", right: "0", equal: true},
		{name: "one huge exponent against itself", left: "1e999999999", right: "1e999999999", equal: true},
		{name: "huge exponents that differ", left: "1e999999999", right: "1e999999998"},
		{name: "beyond double precision", left: "1234567890123456788", right: "1234567890123456789"},
		{name: "sign", left: "-1", right: "1"},
		{name: "coefficient", left: "12", right: "13"},
		{name: "scale", left: "1", right: "10"},
		{name: "zero against a value", left: "0", right: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.equal, numberEqual(json.Number(tc.left), json.Number(tc.right)))
			require.Equal(t, tc.equal, numberEqual(json.Number(tc.right), json.Number(tc.left)))
		})
	}
}

// TestValueEqualityJudgesWholeDecodedForms pins the structural half: objects are
// compared member by member whatever order they arrived in, arrays by position,
// and a value of another shape is never equal to one of this shape.
func TestValueEqualityJudgesWholeDecodedForms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		left  string
		right string
		equal bool
	}{
		{name: "reordered members", left: `{"a":1,"b":2}`, right: `{"b":2,"a":1}`, equal: true},
		{name: "insignificant whitespace", left: `{"a": [1, 2]}`, right: `{"a":[1,2]}`, equal: true},
		{name: "nested numbers by value", left: `{"a":[1.0,{"b":2e1}]}`, right: `{"a":[1,{"b":20}]}`, equal: true},
		{name: "strings and literals", left: `{"a":"x","b":true,"c":null}`, right: `{"a":"x","b":true,"c":null}`, equal: true},
		{name: "extra member", left: `{"a":1}`, right: `{"a":1,"b":2}`},
		{name: "renamed member", left: `{"a":1}`, right: `{"b":1}`},
		{name: "member value", left: `{"a":1}`, right: `{"a":2}`},
		{name: "object against array", left: `{"a":1}`, right: `["a",1]`},
		{name: "array length", left: `[1]`, right: `[1,2]`},
		{name: "array order", left: `[1,2]`, right: `[2,1]`},
		{name: "array against string", left: `[1]`, right: `"1"`},
		{name: "number against string", left: `1`, right: `"1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			left, ok := decodeValue([]byte(tc.left))
			require.True(t, ok)

			right, ok := decodeValue([]byte(tc.right))
			require.True(t, ok)

			require.Equal(t, tc.equal, valueEqual(left, right))
			require.Equal(t, tc.equal, valueEqual(right, left))
		})
	}
}

// TestDecodeValueReadsExactlyOneLosslessValue pins what the comparison basis is
// built from: a literal no float64 can hold still decodes, and a document
// carrying anything after its first value is not one value at all.
func TestDecodeValueReadsExactlyOneLosslessValue(t *testing.T) {
	t.Parallel()

	value, ok := decodeValue([]byte(`{"n":1e999999999}`))
	require.True(t, ok)

	members, isObject := value.(map[string]any)
	require.True(t, isObject)
	require.Equal(t, json.Number("1e999999999"), members["n"])

	_, ok = decodeValue([]byte(`{} {}`))
	require.False(t, ok)

	_, ok = decodeValue([]byte(`{`))
	require.False(t, ok)

	_, ok = decodeValue(nil)
	require.False(t, ok)
}
