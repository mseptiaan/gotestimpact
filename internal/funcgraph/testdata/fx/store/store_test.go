package store

import (
	"encoding/json"
	"testing"
)

func TestJSON(t *testing.T) {
	if _, err := json.Marshal(Model{ID: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestGet(t *testing.T) {
	if _, err := NewMem().Get(1); err != nil {
		t.Fatal(err)
	}
}

func TestLetter(t *testing.T) {
	if Letter(B) != "b" {
		t.Fatal("bad")
	}
}
