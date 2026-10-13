package pgxadapter

import "github.com/jackc/pgx/v5/pgxpool"

func (r IdentityRunner) Raw() *pgxpool.Pool { return r.pool }
