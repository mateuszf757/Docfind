// Package sensitive to wartość, której nie da się wypisać przez przypadek:
// fmt, log i JSON pokazują znacznik zamiast treści, a treść daje tylko
// jawne Reveal. Token root, klucz odzyskiwania i token sesji OpenBao żyją
// w pamięci jako ten typ — błąd w komunikacie albo zrzut struktury
// z %+v nie wynosi ich na terminal ani do logu CI.
package sensitive

import "fmt"

// Mask — co widać w miejscu wartości.
const Mask = "[sekret]"

// Value — wartość poufna.
type Value struct{ v string }

// New opakowuje wartość.
func New(v string) Value { return Value{v: v} }

// Reveal zwraca treść. Jedyne wyjście; każde wywołanie to świadoma decyzja
// o tym, dokąd treść idzie.
func (s Value) Reveal() string { return s.v }

// Empty mówi, czy wartości nie ma.
func (s Value) Empty() bool { return s.v == "" }

func (Value) String() string   { return Mask }
func (Value) GoString() string { return Mask }

// Format obejmuje każdy czasownik fmt, także %x i %q, które omijałyby
// String.
func (Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Mask)) }

// MarshalJSON zapisuje znacznik, nie treść.
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Mask + `"`), nil }

// MarshalText — to samo dla kodowań tekstowych (YAML, logi strukturalne).
func (Value) MarshalText() ([]byte, error) { return []byte(Mask), nil }
