package sensitive

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestNeverPrinted: żaden czasownik fmt, zagnieżdżenie w strukturze, błąd
// ani JSON nie pokazują treści — tylko Reveal.
func TestNeverPrinted(t *testing.T) {
	const secret = "hvs.prawdziwy-token"
	v := New(secret)
	holder := struct {
		Token Value
		Name  string
	}{v, "root"}
	raw, err := json.Marshal(holder)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{
		fmt.Sprint(v), fmt.Sprintf("%v %+v %#v %s %q %x %X %d", v, v, v, v, v, v, v, v),
		fmt.Sprintf("%+v %#v", holder, holder), fmt.Errorf("logowanie: %v", v).Error(),
		errors.New(fmt.Sprint(&v)).Error(), string(raw),
	}
	for _, out := range outputs {
		if strings.Contains(out, secret) || strings.Contains(out, fmt.Sprintf("%x", secret)) {
			t.Errorf("treść wyciekła: %s", out)
		}
	}
	if v.Reveal() != secret || v.Empty() || !New("").Empty() {
		t.Error("Reveal albo Empty nie zwraca treści")
	}
}
