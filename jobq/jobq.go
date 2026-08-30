package jobq

import (
	"context"
	"errors"
	"time"
)

// Status — жизненный цикл задачи.
type Status string

const (
	Pending    Status = "pending"
	Processing Status = "processing"
	Done       Status = "done"
	Failed     Status = "failed"
)

// Job — единица работы.
type Job struct {
	ID         int64
	Key        string         // идемпотентный ключ (напр. "project:42:sha:a1b2c3d...")
	Payload    map[string]any // произвольные данные (project_id, ref, commit_sha...)
	Status     Status
	Attempts   int
	Error      string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// Queue — контракт. Реализации: postgres, mem (тесты), redis (если вырастешь).
type Queue interface {
	// Enqueue добавляет задачу. Идемпотентно: если Key уже существует — не дублирует.
	// Возвращает true, если задача создана; false, если уже была.
	Enqueue(ctx context.Context, key string, payload map[string]any) (bool, error)

	// Claim берёт одну pending-задачу и переводит в processing.
	// Конкурентно-безопасно: два воркера не получат одну и ту же задачу.
	// Возвращает ErrNoJobs, если очередь пуста.
	Claim(ctx context.Context) (*Job, error)

	// Ack — задача выполнена успешно.
	Ack(ctx context.Context, id int64) error

	// Fail — задача упала. Если attempts < max → вернётся в pending (retry).
	// Иначе → failed (мёртвая).
	Fail(ctx context.Context, id int64, errMsg string) error

	// ReclaimStale возвращает в pending задачи, зависшие в processing
	// дольше timeout (воркер упал / OOM / kill). Возвращает число затронутых строк.
	ReclaimStale(ctx context.Context, timeout time.Duration) (int64, error)
}

// Публичные ошибки.
var (
	ErrNoJobs      = errors.New("jobq: no jobs available")
	ErrMaxAttempts = errors.New("jobq: max attempts exceeded")
)
