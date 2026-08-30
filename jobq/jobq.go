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
	Error      string // текст последней ошибки; заполняется при Fail, не возвращается из Claim
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// Stats — снимок количества задач по каждому статусу на момент запроса.
type Stats struct {
	Pending    int
	Processing int
	Done       int
	Failed     int
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
	// Реализация может отложить повторный Claim (backoff): pgq делает это
	// через Config.RetryBase/RetryCap, memq — через SetRetryBase.
	Fail(ctx context.Context, id int64, errMsg string) error

	// Heartbeat продлевает аренду (lease) задачи в processing, чтобы
	// ReclaimStale не отобрал её у живого воркера. Возвращает ErrNotProcessing,
	// если задача не в processing или аренда уже истекла.
	Heartbeat(ctx context.Context, id int64) error

	// ReclaimStale возвращает в pending задачи, зависшие в processing
	// дольше timeout (воркер упал / OOM / kill). Возвращает число затронутых строк.
	// timeout <= 0 означает «использовать дефолтный порог реализации»
	// (pgq — StaleAfter из Config; memq — 5 минут).
	ReclaimStale(ctx context.Context, timeout time.Duration) (int64, error)

	// Stats возвращает количество задач по каждому статусу на момент запроса.
	Stats(ctx context.Context) (Stats, error)

	// Depth возвращает количество задач, которые Claim может выдать прямо сейчас
	// (pending и не исчерпавшие попытки). Это подмножество Stats.Pending.
	Depth(ctx context.Context) (int, error)
}

// Публичные ошибки.
var ErrNoJobs = errors.New("jobq: no jobs available")
