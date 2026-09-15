package sqlite

import (
	"database/sql"

	"github.com/johnlanda/hop/internal/app"
)

// WriteDB exposes the write pool to adapter tests that need raw SQL:
// seeding schema versions, racing constraint inserts from separate handles,
// and inspecting rows no port reads back.
func WriteDB(s *Store) *sql.DB { return s.writes }

// ReadDB exposes the read pool to the connection-churn PRAGMA test.
func ReadDB(s *Store) *sql.DB { return s.reads }

// TxOf exposes a unit of work's transaction, so a test can mutate the lease
// row inside the transaction and prove Commit's re-read rejects a lease
// that is no longer held — a window no port write can open, because every
// other writer serializes behind the open immediate transaction.
func TxOf(u app.UnitOfWork) *sql.Tx {
	uow, ok := u.(*unitOfWork)
	if !ok {
		return nil
	}
	return uow.tx
}
