package bindcode

import "testing"

// Fixed vectors for cross-implementation checks (Kotlin app, web UI). payload
// is the 9 random symbols; check is the Luhn mod 32 check symbol.
var vectors = []struct{ payload, check string }{
	{"000000000", "0"},
	{"012345678", "8"},
	{"ZZZZZZZZZ", "9"},
	{"ABCDEFGHJ", "T"},
	{"KMNPQRSTV", "S"},
	{"WXYZ01234", "Y"},
	{"0Z0Z0Z0Z0", "4"},
	{"7K3M9QX2A", "B"},
}

func TestFixedVectors(t *testing.T) {
	for _, v := range vectors {
		got := string(checkSymbol(v.payload))
		if got != v.check {
			t.Errorf("checkSymbol(%q) = %q want %q", v.payload, got, v.check)
		}
	}
}
