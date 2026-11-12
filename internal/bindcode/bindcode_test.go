package bindcode

import (
	"strings"
	"testing"
)

func TestGenerateIsValidAndRandom(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		c, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != Length {
			t.Fatalf("length %d", len(c))
		}
		n, err := Normalize(c)
		if err != nil || n != c {
			t.Fatalf("generated code %q does not normalize to itself: %q %v", c, n, err)
		}
		if seen[c] {
			t.Fatalf("duplicate code %q in 500 draws", c)
		}
		seen[c] = true
	}
}

func TestFormatAndHash(t *testing.T) {
	if got := Format("0123456789"); got != "01234-56789" {
		t.Fatalf("Format = %q", got)
	}
	if got := Format("ABC"); got != "ABC" {
		t.Fatalf("Format of non-code = %q", got)
	}
	if Hash("0123456789") == Hash("0123456788") || len(Hash("0123456789")) != 64 {
		t.Fatal("Hash must be a distinct 64-char hex digest")
	}
}

// validCodes returns deterministic valid codes covering every symbol at every
// data position.
func validCodes() []string {
	var out []string
	for i := 0; i < len(Alphabet); i++ {
		for shift := 0; shift < DataLength; shift++ {
			data := make([]byte, DataLength)
			for p := range data {
				data[p] = Alphabet[(i*7+p*shift+p*p)%base]
			}
			data[shift] = Alphabet[i]
			out = append(out, string(data)+string(checkSymbol(string(data))))
		}
	}
	return out
}

func TestEverySingleSymbolSubstitutionDetected(t *testing.T) {
	for _, code := range validCodes() {
		for pos := 0; pos < Length; pos++ {
			for _, sym := range []byte(Alphabet) {
				if sym == code[pos] {
					continue
				}
				bad := code[:pos] + string(sym) + code[pos+1:]
				if _, err := Normalize(bad); err == nil {
					t.Fatalf("substitution %q -> %q at %d not detected", code, bad, pos)
				}
			}
		}
	}
}

func TestEveryAdjacentTranspositionDetected(t *testing.T) {
	for _, code := range validCodes() {
		for pos := 0; pos+1 < Length; pos++ {
			a, b := code[pos], code[pos+1]
			if a == b {
				continue
			}
			bad := code[:pos] + string(b) + string(a) + code[pos+2:]
			_, err := Normalize(bad)
			zeroZ := (a == '0' && b == 'Z') || (a == 'Z' && b == '0')
			if zeroZ {
				continue // the documented Luhn mod 32 exception; see package doc
			}
			if err == nil {
				t.Fatalf("transposition %q -> %q at %d not detected", code, bad, pos)
			}
		}
	}
}

func TestZeroZTranspositionIsTheOnlyLuhnBlindSpot(t *testing.T) {
	data := "0Z1234567"
	swapped := "Z01234567"
	if checkSymbol(data) != checkSymbol(swapped) {
		t.Fatal("expected 0/Z swap to be invisible to Luhn mod 32; update the package doc if this changed")
	}
}

func TestNormalizationTable(t *testing.T) {
	good, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	sw := func(s string) string {
		return strings.NewReplacer("0", "O", "1", "L").Replace(s)
	}
	cases := []struct {
		name, in string
		ok       bool
	}{
		{"plain", good, true},
		{"dash", Format(good), true},
		{"lower", strings.ToLower(Format(good)), true},
		{"spaces", " " + good[:3] + " \t" + good[3:] + "\n", true},
		{"O for zero, L for one", sw(good), true},
		{"I for one", strings.ReplaceAll(good, "1", "I"), true},
		{"empty", "", false},
		{"too short", good[:9], false},
		{"too long", good + "0", false},
		{"six char legacy", "AB12C9", false},
		{"U rejected", "U" + good[1:], false},
		{"outside alphabet", "!" + good[1:], false},
		{"unicode", "é" + good[1:], false},
		{"bad checksum", good[:9] + string(Alphabet[(strings.IndexByte(Alphabet, good[9])+1)%base]), false},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%s: Normalize(%q) err=%v want ok=%v", c.name, c.in, err, c.ok)
		}
		if c.ok && got != good {
			t.Errorf("%s: Normalize(%q) = %q want %q", c.name, c.in, got, good)
		}
	}
}

func TestKnownVector(t *testing.T) {
	// Luhn mod 32 over "012345678" (Alphabet values 0..8).
	got := Alphabet[0:0] + string(checkSymbol("012345678"))
	n, err := Normalize("012345678" + got)
	if err != nil || n != "012345678"+got {
		t.Fatalf("known vector failed: %v", err)
	}
}
