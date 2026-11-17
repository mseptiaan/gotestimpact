package svc_test

import (
	"testing"

	"ex.com/fx/store"
	"ex.com/fx/svc"
)

func TestExternalName(t *testing.T) {
	if _, err := svc.New(store.NewMem()).Name(1); err != nil {
		t.Fatal(err)
	}
}
