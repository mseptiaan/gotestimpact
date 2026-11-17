package svc

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ex.com/fx/store"
)

func TestName(t *testing.T) {
	if _, err := New(store.NewMem()).Name(1); err != nil {
		t.Fatal(err)
	}
}

func TestUpper(t *testing.T) {
	// MARK:test-comment
	if Upper("a") != "A" {
		t.Fatal("bad")
	}
}

func TestLabel(t *testing.T) {
	if fmt.Sprint(MakeLabel("x")) != "label:x" {
		t.Fatal("bad")
	}
}

func TestApply(t *testing.T) {
	if Apply([]int{1}, Double)[0] != 2 {
		t.Fatal("bad")
	}
}

func TestMap(t *testing.T) {
	_ = Map([]int{1}, func(i int) int { return i })
}

// A minimal testify-style suite: methods are found by reflection.
type upperSuite struct{ t *testing.T }

func (s *upperSuite) TestShout() {
	if Upper("x") != "X" {
		s.t.Fatal("bad")
	}
}

func runSuite(t *testing.T, s any) {
	v := reflect.ValueOf(s)
	for i := 0; i < v.NumMethod(); i++ {
		if strings.HasPrefix(v.Type().Method(i).Name, "Test") {
			v.Method(i).Call(nil)
		}
	}
}

func TestSuite(t *testing.T) { runSuite(t, &upperSuite{t: t}) }
