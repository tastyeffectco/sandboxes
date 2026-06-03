package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Task is the in-memory mirror of a `task` table row — sandboxd's
// durable record of one coding task (runtimed slice 5). It exists so a
// task's canonical result outlives the sandbox.
type Task struct {
	TaskID            string
	SandboxID         string
	ExternalUserID    sql.NullString
	ExternalProjectID sql.NullString
	Agent             string
	Prompt            string
	Status            string // running | succeeded | failed | cancelled
	ResultJSON        sql.NullString
	CreatedAt         time.Time
	FinishedAt        sql.NullInt64
}

// CreateTask inserts a new task row in the `running` state.
func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO task
			  (task_id, sandbox_id, external_user_id, external_project_id,
			   agent, prompt, status, created_at)
			VALUES (?,?,?,?,?,?,'running',?)`,
			t.TaskID, t.SandboxID, t.ExternalUserID, t.ExternalProjectID,
			t.Agent, t.Prompt, time.Now().Unix())
		return err
	})
}

// FinishTask records a task's terminal status and canonical result.
func (s *Store) FinishTask(ctx context.Context, taskID, status, resultJSON string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			UPDATE task SET status=?, result_json=?, finished_at=?
			 WHERE task_id=?`,
			status, resultJSON, time.Now().Unix(), taskID)
		return err
	})
}

// GetTask returns one task row, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, taskID string) (*Task, error) {
	var t Task
	var created int64
	err := s.db.QueryRowContext(ctx, `
		SELECT task_id, sandbox_id, external_user_id, external_project_id,
		       agent, prompt, status, result_json, created_at, finished_at
		  FROM task WHERE task_id=?`, taskID).Scan(
		&t.TaskID, &t.SandboxID, &t.ExternalUserID, &t.ExternalProjectID,
		&t.Agent, &t.Prompt, &t.Status, &t.ResultJSON, &created, &t.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0).UTC()
	return &t, nil
}

// ListRunningTasks returns every task row still in the `running`
// state — used by boot-time reconciliation.
func (s *Store) ListRunningTasks(ctx context.Context) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, sandbox_id, external_user_id, external_project_id,
		       agent, prompt, status, result_json, created_at, finished_at
		  FROM task WHERE status='running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		var t Task
		var created int64
		if err := rows.Scan(&t.TaskID, &t.SandboxID, &t.ExternalUserID,
			&t.ExternalProjectID, &t.Agent, &t.Prompt, &t.Status,
			&t.ResultJSON, &created, &t.FinishedAt); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, &t)
	}
	return out, rows.Err()
}

// SandboxHasRunningTask reports whether the sandbox has a task still
// running — the idle reaper uses it to avoid reaping mid-task.
func (s *Store) SandboxHasRunningTask(ctx context.Context, sandboxID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM task WHERE sandbox_id=? AND status='running')`,
		sandboxID).Scan(&n)
	return n == 1, err
}
