package store

import "errors"

var ErrNotFound = errors.New("not found")

type Model struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Note string `json:"note"` // MARK:field-note
}

type Repo interface {
	Get(id int) (Model, error)
}

type memRepo struct{ data map[int]Model }

func NewMem() Repo { return &memRepo{data: map[int]Model{1: {ID: 1, Name: "a"}}} }

func (r *memRepo) Get(id int) (Model, error) {
	m, ok := r.data[id] // MARK:get
	if !ok {
		return Model{}, ErrNotFound
	}
	return m, nil
}

func Unused() int { return 42 } // MARK:unused

const (
	A = iota
	B // MARK:const
)

func Letter(i int) string {
	if i == B {
		return "b"
	}
	return "a"
}
