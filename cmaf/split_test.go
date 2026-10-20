package cmaf

import (
	"bytes"
	"testing"
)

func TestSplitInit(t *testing.T) {
	init, _, err := VideoInit(16000, annexB(testSPS, testPPS), nil)
	if err != nil {
		t.Fatal(err)
	}
	frag, _, err := VideoFragment(3, []Sample{{PTS: 0, Key: true, Data: []byte{0, 0, 0, 2, 0x65, 0x88}}}, 667)
	if err != nil {
		t.Fatal(err)
	}
	body := append(append([]byte(nil), init...), frag...)
	a, b, err := SplitInit(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, init) || !bytes.Equal(b, frag) {
		t.Fatalf("split at %d, want %d", len(a), len(init))
	}
	if _, _, err := SplitInit(init); err == nil {
		t.Error("a header without a fragment split")
	}
	if _, _, err := SplitInit(frag); err == nil {
		t.Error("a fragment without a header split")
	}
	if _, _, err := SplitInit(body[:len(init)-3]); err == nil {
		t.Error("a truncated header split")
	}
}
