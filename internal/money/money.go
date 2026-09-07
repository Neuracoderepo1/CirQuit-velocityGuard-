// Package money represents currency as integer micro-cents to avoid
// floating point rounding errors in financial accounting.
package money

import "fmt"

// Micros represents an amount in micro-units of currency (1 unit = 1,000,000 micros).
// Using integers avoids float64 rounding drift across millions of reservations.
type Micros int64

const OneUnit Micros = 1_000_000

// FromFloat converts a float64 dollar amount (e.g. 0.042) to Micros.
// Only use this at system boundaries (parsing config/JSON); do all arithmetic in Micros.
func FromFloat(f float64) Micros {
	return Micros(f * float64(OneUnit))
}

func (m Micros) Float() float64 {
	return float64(m) / float64(OneUnit)
}

func (m Micros) String() string {
	return fmt.Sprintf("$%.4f", m.Float())
}

func (m Micros) Add(o Micros) Micros { return m + o }
func (m Micros) Sub(o Micros) Micros { return m - o }

func Max(a, b Micros) Micros {
	if a > b {
		return a
	}
	return b
}

func Min(a, b Micros) Micros {
	if a < b {
		return a
	}
	return b
}
