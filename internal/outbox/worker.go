// Package outbox delivers platform events with at-least-once semantics.
package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	ID          uuid.UUID
	OrderID     uuid.UUID
	Payload     json.RawMessage
	EndpointURL string
	Attempts    int
}

type Worker struct {
	pool           *pgxpool.Pool
	client         *http.Client
	logger         *slog.Logger
	integrationKey string
	pollInterval   time.Duration
	maxAttempts    int
	parallelism    int
}

func New(pool *pgxpool.Pool, logger *slog.Logger, integrationKey string, pollInterval, httpTimeout time.Duration, maxAttempts int) *Worker {
	return &Worker{
		pool: pool, client: &http.Client{Timeout: httpTimeout}, logger: logger,
		integrationKey: integrationKey, pollInterval: pollInterval, maxAttempts: maxAttempts, parallelism: 4,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if _, err := w.pool.Exec(ctx, `
		UPDATE outbox_events SET status = 'pending', locked_at = NULL, next_attempt_at = now(), updated_at = now(),
			last_error = COALESCE(last_error, 'recovered stale processing event')
		WHERE status = 'processing' AND locked_at < now() - interval '1 minute'`); err != nil {
		return fmt.Errorf("recover stale outbox events: %w", err)
	}
	errCh := make(chan error, w.parallelism)
	for index := 0; index < w.parallelism; index++ {
		go func(workerID int) { errCh <- w.poll(ctx, workerID) }(index)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (w *Worker) poll(ctx context.Context, workerID int) error {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		event, found, err := w.claim(ctx)
		if err != nil {
			w.logger.Error("outbox claim failed", "worker_id", workerID, "error", err)
		} else if found {
			w.deliver(ctx, event)
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *Worker) claim(ctx context.Context) (Event, bool, error) {
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Event{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var event Event
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT e.id
			FROM outbox_events e
			WHERE e.status = 'pending' AND e.next_attempt_at <= now()
			ORDER BY e.next_attempt_at, e.created_at, e.id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE outbox_events e
		SET status = 'processing', attempts = attempts + 1, locked_at = now(), updated_at = now()
		FROM candidate, restaurant_integrations ri, orders o
		WHERE e.id = candidate.id AND o.id = e.aggregate_id AND ri.restaurant_id = o.restaurant_id
		RETURNING e.id, e.aggregate_id, e.payload, ri.order_endpoint_url, e.attempts`).
		Scan(&event.ID, &event.OrderID, &event.Payload, &event.EndpointURL, &event.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("claim event: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET dispatch_started_at = COALESCE(dispatch_started_at, now()), updated_at = now() WHERE id = $1`, event.OrderID); err != nil {
		return Event{}, false, fmt.Errorf("mark order dispatch started: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, false, err
	}
	return event, true, nil
}

func (w *Worker) deliver(ctx context.Context, event Event) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, event.EndpointURL, bytes.NewReader(event.Payload))
	if err != nil {
		w.fail(ctx, event, err, false)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+w.integrationKey)
	request.Header.Set("X-Request-ID", event.ID.String())
	response, err := w.client.Do(request)
	if err != nil {
		w.fail(ctx, event, err, true)
		return
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		w.fail(ctx, event, readErr, true)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		w.fail(ctx, event, fmt.Errorf("restaurant returned HTTP %d: %s", response.StatusCode, string(body)), retryable)
		return
	}
	var decision struct {
		Status    string `json:"status"`
		Rejection *struct {
			Message string `json:"message"`
		} `json:"rejection"`
	}
	if err := json.Unmarshal(body, &decision); err != nil || (decision.Status != "accepted" && decision.Status != "rejected") {
		w.fail(ctx, event, fmt.Errorf("invalid restaurant decision: %s", string(body)), true)
		return
	}
	var reason *string
	if decision.Rejection != nil {
		reason = &decision.Rejection.Message
	}
	if err := w.complete(ctx, event, decision.Status, reason); err != nil {
		w.logger.Error("complete outbox event failed", "event_id", event.ID, "order_id", event.OrderID, "error", err)
		return
	}
	w.logger.Info("outbox event delivered", "event_id", event.ID, "order_id", event.OrderID, "attempt", event.Attempts)
}

func (w *Worker) complete(ctx context.Context, event Event, decision string, reason *string) error {
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var eventStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM outbox_events WHERE id = $1 FOR UPDATE`, event.ID).Scan(&eventStatus); err != nil {
		return err
	}
	if eventStatus == "processed" {
		return tx.Commit(ctx)
	}
	if eventStatus != "processing" {
		return fmt.Errorf("event has unexpected status %q", eventStatus)
	}
	var orderStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1 FOR UPDATE`, event.OrderID).Scan(&orderStatus); err != nil {
		return err
	}
	if orderStatus == "pending_confirmation" {
		if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2, updated_at = now() WHERE id = $1`, event.OrderID, decision); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO order_status_history (order_id, status, actor, reason) VALUES ($1, $2, 'worker', $3)`, event.OrderID, decision, reason); err != nil {
			return err
		}
	} else if orderStatus != decision {
		return fmt.Errorf("order has unexpected status %q for decision %q", orderStatus, decision)
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox_events SET status = 'processed', processed_at = now(), locked_at = NULL, updated_at = now(), last_error = NULL WHERE id = $1`, event.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (w *Worker) fail(ctx context.Context, event Event, deliveryErr error, retryable bool) {
	status := "pending"
	if !retryable || event.Attempts >= w.maxAttempts {
		status = "exhausted"
	}
	delay := Backoff(event.Attempts)
	_, err := w.pool.Exec(ctx, `
		UPDATE outbox_events SET status = $2, next_attempt_at = now() + $3::interval,
			locked_at = NULL, last_error = $4, updated_at = now()
		WHERE id = $1 AND status = 'processing'`, event.ID, status, delay.String(), deliveryErr.Error())
	if err != nil {
		w.logger.Error("record outbox failure failed", "event_id", event.ID, "order_id", event.OrderID, "error", err)
		return
	}
	w.logger.Warn("outbox delivery failed", "event_id", event.ID, "order_id", event.OrderID, "attempt", event.Attempts, "status", status, "retry_in", delay, "error", deliveryErr)
}

func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Second * time.Duration(1<<(attempt-1))
}
