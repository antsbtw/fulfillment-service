package repository

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsLiveProvisionConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"live index violation", &pgconn.PgError{Code: "23505", ConstraintName: uqHostingProvUserLive}, true},
		{"wrapped", fmt.Errorf("exec: %w", &pgconn.PgError{Code: "23505", ConstraintName: uqHostingProvUserLive}), true},
		{"other unique index", &pgconn.PgError{Code: "23505", ConstraintName: "hosting_provisions_pkey"}, false},
		{"other pg error", &pgconn.PgError{Code: "23503", ConstraintName: uqHostingProvUserLive}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isLiveProvisionConflict(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
