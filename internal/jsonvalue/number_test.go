package jsonvalue

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInt64PreservesExactJSONValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		wire string
		want int64
		ok   bool
	}{
		{"1", 1, true}, {"1.0", 1, true}, {"10e-1", 1, true}, {"0.001E3", 1, true},
		{"-0e999999999999999999999", 0, true},
		{"9223372036854775807", math.MaxInt64, true},
		{"92233720368547758070e-1", math.MaxInt64, true},
		{"-9223372036854775808", math.MinInt64, true},
		{"9223372036854775808", 0, false}, {"-9223372036854775809", 0, false},
		{"1.0000000000000001", 0, false}, {"-1e-400", 0, false},
		{"1e400", 0, false}, {"1e999999999999999999999", 0, false},
		{"1e-999999999999999999999", 0, false},
		{"01", 0, false}, {"NaN", 0, false}, {"null", 0, false}, {"", 0, false},
	} {
		t.Run(test.wire, func(t *testing.T) {
			t.Parallel()
			got, ok := Int64(json.Number(test.wire))
			require.Equal(t, test.ok, ok)
			if ok {
				require.Equal(t, test.want, got)
			}
		})
	}
}
