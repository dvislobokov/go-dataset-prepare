// Package store is a tiny in-module dependency used by semantic tests.
package store

import (
	"context"
	"errors"
	"sync"
)

// ErrNotFound is returned when a user does not exist.
var ErrNotFound = errors.New("store: not found")

// User is a stored user.
type User struct {
	ID    int
	Name  string
	Email string
	tags  []string
}

// Store keeps users in memory.
type Store struct {
	mu    sync.Mutex
	users map[int]*User
}

// New creates an empty store.
func New() *Store { return &Store{users: map[int]*User{}} }

// GetUser returns the user with the given id.
func (s *Store) GetUser(ctx context.Context, id int) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

// PutUser stores u.
func (s *Store) PutUser(ctx context.Context, u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
	return nil
}
