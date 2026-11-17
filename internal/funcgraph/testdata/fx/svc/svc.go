package svc

import (
	"fmt"
	"strings"

	"ex.com/fx/store"
)

type Service struct{ repo store.Repo }

func New(r store.Repo) *Service { return &Service{repo: r} }

func (s *Service) Name(id int) (string, error) {
	m, err := s.repo.Get(id)
	if err != nil {
		return "", err
	}
	return m.Name, nil
}

func Upper(s string) string {
	return strings.ToUpper(s) // MARK:upper
}

type Label struct{ v string }

func (l Label) String() string {
	return "label:" + l.v // MARK:string
}

func MakeLabel(v string) Label { return Label{v: v} }

func Apply(xs []int, f func(int) int) []int {
	out := make([]int, len(xs))
	for i, x := range xs {
		out[i] = f(x)
	}
	return out
}

func Double(x int) int {
	return x * 2 // MARK:double
}

func Map[T any](xs []T, f func(T) T) []T {
	out := make([]T, 0, len(xs)) // MARK:generic
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

var registry = map[string]int{}

var _ fmt.Stringer = (*Label)(nil) // MARK:assert

func init() {
	registry["x"] = 1 // MARK:init
}
