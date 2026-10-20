package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

func TestUpdateUpstreamBalanceProbeSnapshotMergesOnlyBalanceKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	snapshot := &service.UpstreamBalanceProbeSnapshot{
		Status:        service.UpstreamBillingProbeStatusOK,
		Data:          map[string]any{"kind": service.UpstreamBalanceKindSub2API, "remaining_usd": 12.5},
		ReceivedAt:    &now,
		FreshUntil:    &now,
		LastAttemptAt: now,
		NextProbeAt:   now,
	}
	account := &service.Account{
		ID:          27,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{},
	}

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts.*COALESCE\(extra, '\{\}'::jsonb\) \|\| \$1::jsonb.*COALESCE\(extra -> 'upstream_balance_probe', 'null'::jsonb\) = \$7::jsonb.*deleted_at IS NULL`).
		WithArgs(sqlmock.AnyArg(), int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil, `null`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	require.NoError(t, repo.UpdateUpstreamBalanceProbeSnapshot(context.Background(), account, snapshot))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateUpstreamBalanceProbeSnapshotRejectsStaleIdentity(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	account := &service.Account{
		ID:          27,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{},
	}

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts.*upstream_balance_probe`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	err = repo.UpdateUpstreamBalanceProbeSnapshot(context.Background(), account, &service.UpstreamBalanceProbeSnapshot{
		Status:        service.UpstreamBillingProbeStatusOK,
		LastAttemptAt: now,
		NextProbeAt:   now,
	})
	require.ErrorIs(t, err, service.ErrUpstreamBillingProbeIdentityChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}
