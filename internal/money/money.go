// Package money provides a thin Money wrapper around shopspring/decimal.
// Scale is enforced at the Money boundary: all constructors round to 4 decimal
// places matching the DB column NUMERIC(20,4).
package money

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Money represents a monetary amount with a fixed 4-decimal-place scale.
// The zero value is valid and equals zero money.
type Money struct {
	d decimal.Decimal
}

// FromString parses s as a decimal and rounds to 4 decimal places.
// Returns an error if s is not a valid decimal string.
func FromString(s string) (Money, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Money{}, fmt.Errorf("money.FromString: %w", err)
	}
	return Money{d: d.Round(4)}, nil
}

// FromInt creates a Money value from an integer, with no fractional part.
func FromInt(n int64) Money {
	return Money{d: decimal.NewFromInt(n).Round(4)}
}

// Zero returns a Money value of zero.
func Zero() Money {
	return Money{d: decimal.Zero.Round(4)}
}

// Add returns the sum of m and other.
func (m Money) Add(other Money) Money {
	return Money{d: m.d.Add(other.d).Round(4)}
}

// Sub returns m minus other.
func (m Money) Sub(other Money) Money {
	return Money{d: m.d.Sub(other.d).Round(4)}
}

// Neg returns the negation of m.
func (m Money) Neg() Money {
	return Money{d: m.d.Neg().Round(4)}
}

// IsPositive reports whether m is strictly greater than zero.
func (m Money) IsPositive() bool {
	return m.d.IsPositive()
}

// IsZero reports whether m equals zero.
func (m Money) IsZero() bool {
	return m.d.IsZero()
}

// LessThan reports whether m is strictly less than other.
func (m Money) LessThan(other Money) bool {
	return m.d.LessThan(other.d)
}

// Equal reports whether m equals other.
func (m Money) Equal(other Money) bool {
	return m.d.Equal(other.d)
}

// String returns a fixed 4-decimal-place representation, e.g. "100.5000".
func (m Money) String() string {
	return m.d.StringFixed(4)
}

// Decimal returns the underlying decimal.Decimal value for DB scanning.
func (m Money) Decimal() decimal.Decimal {
	return m.d
}

// MarshalJSON serializes Money as a quoted decimal string, e.g. "100.5000".
func (m Money) MarshalJSON() ([]byte, error) {
	return []byte(`"` + m.String() + `"`), nil
}

// UnmarshalJSON deserializes a quoted decimal string into Money, rounding to 4dp.
func (m *Money) UnmarshalJSON(b []byte) error {
	// Strip surrounding quotes if present.
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	parsed, err := FromString(s)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

// NewFromDecimal wraps an existing decimal.Decimal, rounding to 4dp.
// Used internally by the store layer when scanning DB rows.
func NewFromDecimal(d decimal.Decimal) Money {
	return Money{d: d.Round(4)}
}
