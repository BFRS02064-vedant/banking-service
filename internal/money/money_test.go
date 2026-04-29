package money

import (
	"encoding/json"
	"testing"
)

func TestFromString_RoundTrip(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"100.5", "100.5000"},
		{"100.12345", "100.1235"}, // 5dp rounds to 4dp
		{"0", "0.0000"},
		{"0.0", "0.0000"},
		{"-50.25", "-50.2500"},
		{"999999999999999.9999", "999999999999999.9999"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			m, err := FromString(tc.input)
			if err != nil {
				t.Fatalf("FromString(%q) error: %v", tc.input, err)
			}
			if got := m.String(); got != tc.want {
				t.Errorf("FromString(%q).String() = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestFromString_Invalid(t *testing.T) {
	_, err := FromString("not-a-number")
	if err == nil {
		t.Fatal("expected error for invalid decimal string")
	}
}

func TestFromInt(t *testing.T) {
	m := FromInt(10000)
	if got := m.String(); got != "10000.0000" {
		t.Errorf("FromInt(10000).String() = %q; want %q", got, "10000.0000")
	}
}

func TestZero(t *testing.T) {
	m := Zero()
	if !m.IsZero() {
		t.Error("Zero().IsZero() should be true")
	}
	if m.IsPositive() {
		t.Error("Zero().IsPositive() should be false")
	}
}

func TestArithmetic(t *testing.T) {
	a, _ := FromString("100.0000")
	b, _ := FromString("50.0000")

	t.Run("Add", func(t *testing.T) {
		got := a.Add(b)
		if got.String() != "150.0000" {
			t.Errorf("Add = %s; want 150.0000", got.String())
		}
	})

	t.Run("Sub", func(t *testing.T) {
		got := a.Sub(b)
		if got.String() != "50.0000" {
			t.Errorf("Sub = %s; want 50.0000", got.String())
		}
	})

	t.Run("Neg", func(t *testing.T) {
		got := a.Neg()
		if got.String() != "-100.0000" {
			t.Errorf("Neg = %s; want -100.0000", got.String())
		}
	})

	t.Run("SubToNegative", func(t *testing.T) {
		// Money.Sub does not reject negatives; that is the service layer's job.
		got := b.Sub(a)
		if got.String() != "-50.0000" {
			t.Errorf("Sub(b,a) = %s; want -50.0000", got.String())
		}
	})
}

func TestIsPositiveIsZero_Boundaries(t *testing.T) {
	zero := Zero()
	pos, _ := FromString("0.0001")
	neg, _ := FromString("-0.0001")

	if !zero.IsZero() {
		t.Error("zero.IsZero() should be true")
	}
	if zero.IsPositive() {
		t.Error("zero.IsPositive() should be false")
	}
	if !pos.IsPositive() {
		t.Error("0.0001.IsPositive() should be true")
	}
	if neg.IsPositive() {
		t.Error("-0.0001.IsPositive() should be false")
	}
}

func TestLessThanEqual(t *testing.T) {
	a, _ := FromString("10")
	b, _ := FromString("20")

	if !a.LessThan(b) {
		t.Error("10 < 20 should be true")
	}
	if b.LessThan(a) {
		t.Error("20 < 10 should be false")
	}
	if !a.Equal(a) {
		t.Error("a.Equal(a) should be true")
	}
	if a.Equal(b) {
		t.Error("10.Equal(20) should be false")
	}
}

func TestMarshalJSON(t *testing.T) {
	m, _ := FromString("99.9")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("MarshalJSON error: %v", err)
	}
	if string(b) != `"99.9000"` {
		t.Errorf("MarshalJSON = %s; want %q", string(b), `"99.9000"`)
	}
}

func TestUnmarshalJSON(t *testing.T) {
	var m Money
	if err := json.Unmarshal([]byte(`"100.5"`), &m); err != nil {
		t.Fatalf("UnmarshalJSON error: %v", err)
	}
	if m.String() != "100.5000" {
		t.Errorf("UnmarshalJSON = %s; want 100.5000", m.String())
	}
}

func TestPtrMoneyNilMarshal(t *testing.T) {
	type payload struct {
		Amount *Money `json:"amount"`
	}
	p := payload{Amount: nil}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if string(b) != `{"amount":null}` {
		t.Errorf("nil *Money marshal = %s; want {\"amount\":null}", string(b))
	}
}

func TestScaleEnforcedAtConstructor(t *testing.T) {
	// Verify rounding: banker's rounding is NOT used; shopspring rounds half-up.
	m, _ := FromString("100.12345")
	if m.String() != "100.1235" {
		t.Errorf("5dp input = %s; want 100.1235", m.String())
	}
	// Confirm Add also preserves 4dp.
	a, _ := FromString("0.00001")
	z := Zero().Add(a)
	if z.String() != "0.0000" {
		t.Errorf("0 + 0.00001 rounded to 4dp = %s; want 0.0000", z.String())
	}
}
