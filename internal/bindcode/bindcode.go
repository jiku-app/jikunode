// Package bindcode is the single implementation of the node binding code: a
// 10-symbol Crockford base32 string (9 random symbols plus one Luhn mod 32
// check symbol) shown as XXXXX-XXXXX. It is a leaf package (no database, no
// network) so the hub, supply, node agent and mobile bindings share it.
//
// The check symbol exists for users only: it lets the CLI, the app and the hub
// reject a mistyped code before any database lookup. Attackers can compute it,
// so it adds no security.
//
// Known edge case: Luhn mod 32 detects every single-symbol substitution and
// every adjacent transposition except swapping the symbols '0' and 'Z'
// (values 0 and 31), which collide under the doubling step. A code mistyped
// that way passes the local check and fails the database lookup instead.
package bindcode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	// Alphabet is Crockford base32 without the check-only extras (no I, L, O, U).
	Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	// Length is the number of symbols in a code, check symbol included.
	Length = 10
	// DataLength is the number of random symbols.
	DataLength = Length - 1
	base       = len(Alphabet)
)

// ErrInvalid is returned by Normalize for any malformed code.
var ErrInvalid = errors.New("bindcode: invalid code")

// Generate returns a new code (10 symbols, no dash) from crypto/rand.
func Generate() (string, error) {
	raw := make([]byte, DataLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	data := make([]byte, DataLength, Length)
	for i, b := range raw {
		data[i] = Alphabet[int(b)%base] // base divides 256: no modulo bias
	}
	return string(append(data, checkSymbol(string(data)))), nil
}

// checkSymbol computes the Luhn mod 32 check symbol over data (all symbols
// must be in Alphabet).
func checkSymbol(data string) byte {
	factor, sum := 2, 0
	for i := len(data) - 1; i >= 0; i-- {
		addend := factor * strings.IndexByte(Alphabet, data[i])
		if factor == 2 {
			factor = 1
		} else {
			factor = 2
		}
		sum += addend/base + addend%base
	}
	return Alphabet[(base-sum%base)%base]
}

// Normalize returns the canonical 10-symbol form of user input: whitespace and
// '-' stripped, upper-cased, O mapped to 0, I and L mapped to 1. It rejects U,
// anything outside the alphabet, any other length, and a bad check symbol.
func Normalize(input string) (string, error) {
	out := make([]byte, 0, Length)
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch {
		case c == '-' || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			continue
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		}
		switch c {
		case 'O':
			c = '0'
		case 'I', 'L':
			c = '1'
		}
		if strings.IndexByte(Alphabet, c) < 0 || len(out) == Length {
			return "", ErrInvalid
		}
		out = append(out, c)
	}
	if len(out) != Length || out[DataLength] != checkSymbol(string(out[:DataLength])) {
		return "", ErrInvalid
	}
	return string(out), nil
}

// Format renders a normalized code as XXXXX-XXXXX. Any other input is
// returned unchanged.
func Format(code string) string {
	if len(code) != Length {
		return code
	}
	return code[:5] + "-" + code[5:]
}

// Hash returns the lookup key of a normalized code: lowercase hex SHA-256.
func Hash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
