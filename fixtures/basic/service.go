package basic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"example.com/basic/internal/store"
	"github.com/ext/lib"
)

// Service wires the store and a logger.
type Service struct {
	store   *store.Store
	logger  *slog.Logger
	timeout time.Duration
}

// Config holds options.
type Config struct {
	Name    string
	Retries int
	Labels  map[string]string
}

// NewService builds a Service.
func NewService(s *store.Store, logger *slog.Logger) *Service {
	return &Service{store: s, logger: logger, timeout: 5 * time.Second}
}

// Rename changes a user's name.
func (svc *Service) Rename(ctx context.Context, id int, name string) (*store.User, error) {
	ctx, cancel := context.WithTimeout(ctx, svc.timeout)
	defer cancel()
	user, err := svc.store.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("rename %d: %w", id, err)
		}
		return nil, err
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		svc.logger.Info("empty name", "id", id)
		return nil, errors.New("empty name")
	}
	user.Name = trimmed
	if err := svc.store.PutUser(ctx, user); err != nil {
		return nil, fmt.Errorf("put user %d: %w", user.ID, err)
	}
	return user, nil
}

// Fanout runs work concurrently.
func (svc *Service) Fanout(items []string) []string {
	results := make(chan string, len(items))
	for _, it := range items {
		go func(v string) {
			results <- strings.ToUpper(v)
		}(it)
	}
	out := make([]string, 0, len(items))
	for range items {
		select {
		case r := <-results:
			out = append(out, r)
		case <-time.After(time.Second):
			return out
		}
	}
	cfg := Config{
		Name:    "fanout",
		Retries: 3,
		Labels:  map[string]string{"a": "b"},
	}
	_ = cfg
	return out
}

// Map applies f to each element.
func Map[T, U any](xs []T, f func(T) U) []U {
	out := make([]U, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

func useExternal() string {
	c := lib.NewClient("x")
	return c.Name()
}

const banner = `multi
line raw string`

/*
block comment
spanning lines
*/
func describe(n int) string {
	switch {
	case n < 0:
		return "negative"
	default:
		return fmt.Sprintf("n=%d", n)
	}
}
