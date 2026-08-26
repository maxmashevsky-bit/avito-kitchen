package orders

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/model"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/serviceerror"
)

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

type CreateResult struct {
	Order    model.Order
	Replayed bool
}

func (s *Service) Create(ctx context.Context, userID, restaurantID uuid.UUID, idempotencyKey string, requested []RequestedItem) (CreateResult, error) {
	if userID == uuid.Nil || restaurantID == uuid.Nil || idempotencyKey == "" || len(idempotencyKey) > 128 || len(requested) == 0 {
		return CreateResult{}, serviceerror.New("VALIDATION_ERROR", "user, restaurant, idempotency key and items are required")
	}
	seen := make(map[uuid.UUID]struct{}, len(requested))
	for _, item := range requested {
		if item.MenuItemID == uuid.Nil || item.Quantity <= 0 || item.Quantity > 100 || item.SeenUnitPriceMinor <= 0 {
			return CreateResult{}, serviceerror.New("VALIDATION_ERROR", "each order item must have an ID, positive quantity and positive seen price")
		}
		if _, duplicate := seen[item.MenuItemID]; duplicate {
			return CreateResult{}, serviceerror.New("VALIDATION_ERROR", "menu_item_id must not repeat")
		}
		seen[item.MenuItemID] = struct{}{}
	}
	requestHash, err := RequestHash(restaurantID, requested)
	if err != nil {
		return CreateResult{}, fmt.Errorf("hash order request: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CreateResult{}, fmt.Errorf("begin create order: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if existing, existingHash, found, err := findIdempotent(ctx, tx, userID, idempotencyKey); err != nil {
		return CreateResult{}, err
	} else if found {
		if !bytes.Equal(existingHash, requestHash[:]) {
			return CreateResult{}, serviceerror.New("IDEMPOTENCY_KEY_REUSED", "idempotency key was already used with another request")
		}
		order, err := loadOrder(ctx, tx, existing, userID, nil)
		return CreateResult{Order: order, Replayed: true}, err
	}

	var restaurantName string
	var accepting bool
	err = tx.QueryRow(ctx, `SELECT name, is_accepting_orders FROM restaurants WHERE id = $1 FOR SHARE`, restaurantID).Scan(&restaurantName, &accepting)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreateResult{}, serviceerror.New("NOT_FOUND", "restaurant not found")
	}
	if err != nil {
		return CreateResult{}, fmt.Errorf("load restaurant: %w", err)
	}
	if !accepting {
		return CreateResult{}, serviceerror.New("RESTAURANT_CLOSED", "restaurant is not accepting orders")
	}

	ids := make([]uuid.UUID, 0, len(requested))
	for _, item := range requested {
		ids = append(ids, item.MenuItemID)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, restaurant_id, external_item_id, name, unit_price_minor, currency, available
		FROM menu_items WHERE id = ANY($1) ORDER BY id FOR SHARE`, ids)
	if err != nil {
		return CreateResult{}, fmt.Errorf("load requested items: %w", err)
	}
	catalogItems := make([]CatalogItem, 0, len(requested))
	for rows.Next() {
		var item CatalogItem
		if err := rows.Scan(&item.ID, &item.RestaurantID, &item.ExternalItemID, &item.Name, &item.UnitPriceMinor, &item.Currency, &item.Available); err != nil {
			rows.Close()
			return CreateResult{}, fmt.Errorf("scan requested item: %w", err)
		}
		catalogItems = append(catalogItems, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return CreateResult{}, fmt.Errorf("iterate requested items: %w", err)
	}
	orderItems, total, err := BuildOrderItems(restaurantID, requested, catalogItems)
	if err != nil {
		var ruleErr *RuleError
		if !errors.As(err, &ruleErr) {
			return CreateResult{}, err
		}
		switch ruleErr.Code {
		case "MULTIPLE_RESTAURANTS":
			return CreateResult{}, serviceerror.New(ruleErr.Code, "all items must belong to the selected restaurant")
		case "ITEM_UNAVAILABLE":
			return CreateResult{}, serviceerror.WithDetails(ruleErr.Code, "menu item is unavailable", map[string]any{"menu_item_id": ruleErr.MenuItemID})
		case "PRICE_CHANGED":
			return CreateResult{}, serviceerror.WithDetails(ruleErr.Code, "menu item price has changed", map[string]any{"menu_item_id": ruleErr.MenuItemID, "current_unit_price_minor": ruleErr.CurrentUnitPriceMinor})
		case "NOT_FOUND":
			return CreateResult{}, serviceerror.New(ruleErr.Code, "one or more menu items were not found")
		default:
			return CreateResult{}, serviceerror.New("VALIDATION_ERROR", "order total is too large")
		}
	}

	orderID := uuid.New()
	createdAt := time.Now().UTC()
	var insertedID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO orders (id, user_id, restaurant_id, restaurant_name_snapshot, idempotency_key, request_hash, status, total_price_minor, currency, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending_confirmation', $7, 'RUB', $8, $8)
		ON CONFLICT (user_id, idempotency_key) DO NOTHING
		RETURNING id`, orderID, userID, restaurantID, restaurantName, idempotencyKey, requestHash[:], total, createdAt).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, existingHash, found, findErr := findIdempotent(ctx, tx, userID, idempotencyKey)
		if findErr != nil {
			return CreateResult{}, findErr
		}
		if !found || !bytes.Equal(existingHash, requestHash[:]) {
			return CreateResult{}, serviceerror.New("IDEMPOTENCY_KEY_REUSED", "idempotency key was already used with another request")
		}
		order, loadErr := loadOrder(ctx, tx, existing, userID, nil)
		if loadErr != nil {
			return CreateResult{}, loadErr
		}
		if err := tx.Commit(ctx); err != nil {
			return CreateResult{}, fmt.Errorf("commit idempotent replay: %w", err)
		}
		return CreateResult{Order: order, Replayed: true}, nil
	}
	if err != nil {
		return CreateResult{}, fmt.Errorf("insert order: %w", err)
	}
	for _, item := range orderItems {
		_, err = tx.Exec(ctx, `
			INSERT INTO order_items (order_id, menu_item_id, external_item_id, name_snapshot, unit_price_minor, quantity, line_total_minor, currency)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, orderID, item.MenuItemID, item.ExternalItemID, item.NameSnapshot, item.UnitPriceMinor, item.Quantity, item.LineTotalMinor, item.Currency)
		if err != nil {
			return CreateResult{}, fmt.Errorf("insert order item: %w", err)
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO order_status_history (order_id, status, actor, changed_at) VALUES ($1, 'pending_confirmation', 'user', $2)`, orderID, createdAt)
	if err != nil {
		return CreateResult{}, fmt.Errorf("insert initial order history: %w", err)
	}
	payloadItems := make([]map[string]any, 0, len(orderItems))
	for _, item := range orderItems {
		payloadItems = append(payloadItems, map[string]any{
			"platform_menu_item_id": item.MenuItemID, "external_item_id": item.ExternalItemID,
			"name": item.NameSnapshot, "unit_price_minor": item.UnitPriceMinor, "quantity": item.Quantity,
			"line_total_minor": item.LineTotalMinor, "currency": item.Currency,
		})
	}
	payload, err := json.Marshal(map[string]any{
		"platform_order_id": orderID, "platform_restaurant_id": restaurantID, "created_at": createdAt,
		"total_price_minor": total, "currency": "RUB", "items": payloadItems,
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("encode outbox payload: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload)
		VALUES ('order', $1, 'order.created', $2)`, orderID, payload)
	if err != nil {
		return CreateResult{}, fmt.Errorf("insert order outbox event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CreateResult{}, fmt.Errorf("commit create order: %w", err)
	}
	order := model.Order{ID: orderID, UserID: userID, RestaurantID: restaurantID, RestaurantNameSnapshot: restaurantName, Status: PendingConfirmation, TotalPriceMinor: total, Currency: "RUB", CreatedAt: createdAt, UpdatedAt: createdAt, Items: orderItems, History: []model.OrderHistory{{Status: PendingConfirmation, Actor: "user", ChangedAt: createdAt}}}
	return CreateResult{Order: order}, nil
}

func findIdempotent(ctx context.Context, tx pgx.Tx, userID uuid.UUID, key string) (uuid.UUID, []byte, bool, error) {
	var id uuid.UUID
	var hash []byte
	err := tx.QueryRow(ctx, `SELECT id, request_hash FROM orders WHERE user_id = $1 AND idempotency_key = $2 FOR UPDATE`, userID, key).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil, false, nil
	}
	if err != nil {
		return uuid.Nil, nil, false, fmt.Errorf("find idempotent order: %w", err)
	}
	return id, hash, true, nil
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadOrder(ctx context.Context, db queryer, orderID, userID uuid.UUID, restaurantID *uuid.UUID) (model.Order, error) {
	var order model.Order
	err := db.QueryRow(ctx, `
		SELECT id, user_id, restaurant_id, restaurant_name_snapshot, status, total_price_minor, currency, created_at, updated_at
		FROM orders WHERE id = $1 AND ($2::uuid = '00000000-0000-0000-0000-000000000000' OR user_id = $2)
		AND ($3::uuid IS NULL OR restaurant_id = $3)`, orderID, userID, restaurantID).
		Scan(&order.ID, &order.UserID, &order.RestaurantID, &order.RestaurantNameSnapshot, &order.Status, &order.TotalPriceMinor, &order.Currency, &order.CreatedAt, &order.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Order{}, serviceerror.New("NOT_FOUND", "order not found")
	}
	if err != nil {
		return model.Order{}, fmt.Errorf("get order: %w", err)
	}
	rows, err := db.Query(ctx, `SELECT menu_item_id, external_item_id, name_snapshot, unit_price_minor, quantity, line_total_minor, currency FROM order_items WHERE order_id = $1 ORDER BY id`, orderID)
	if err != nil {
		return model.Order{}, fmt.Errorf("get order items: %w", err)
	}
	for rows.Next() {
		var item model.OrderItem
		if err := rows.Scan(&item.MenuItemID, &item.ExternalItemID, &item.NameSnapshot, &item.UnitPriceMinor, &item.Quantity, &item.LineTotalMinor, &item.Currency); err != nil {
			rows.Close()
			return model.Order{}, fmt.Errorf("scan order item: %w", err)
		}
		order.Items = append(order.Items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return model.Order{}, err
	}
	historyRows, err := db.Query(ctx, `SELECT status, actor, reason, changed_at FROM order_status_history WHERE order_id = $1 ORDER BY changed_at, id`, orderID)
	if err != nil {
		return model.Order{}, fmt.Errorf("get order history: %w", err)
	}
	defer historyRows.Close()
	for historyRows.Next() {
		var item model.OrderHistory
		if err := historyRows.Scan(&item.Status, &item.Actor, &item.Reason, &item.ChangedAt); err != nil {
			return model.Order{}, fmt.Errorf("scan order history: %w", err)
		}
		order.History = append(order.History, item)
	}
	return order, historyRows.Err()
}

func (s *Service) Get(ctx context.Context, orderID, userID uuid.UUID) (model.Order, error) {
	return loadOrder(ctx, s.pool, orderID, userID, nil)
}

func (s *Service) GetPartner(ctx context.Context, orderID, restaurantID uuid.UUID) (model.Order, error) {
	return loadOrder(ctx, s.pool, orderID, uuid.Nil, &restaurantID)
}

type orderCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

func (s *Service) ListUser(ctx context.Context, userID uuid.UUID, limit int, cursor string) ([]model.Order, string, error) {
	return s.list(ctx, &userID, nil, "", limit, cursor)
}

func (s *Service) ListPartner(ctx context.Context, restaurantID uuid.UUID, status string, limit int, cursor string) ([]model.Order, string, error) {
	return s.list(ctx, nil, &restaurantID, status, limit, cursor)
}

func (s *Service) list(ctx context.Context, userID, restaurantID *uuid.UUID, status string, limit int, cursor string) ([]model.Order, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", serviceerror.New("VALIDATION_ERROR", "limit must be between 1 and 100")
	}
	position := orderCursor{}
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(decoded, &position) != nil {
			return nil, "", serviceerror.New("VALIDATION_ERROR", "invalid cursor")
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, restaurant_id, restaurant_name_snapshot, status, total_price_minor, currency, created_at, updated_at
		FROM orders
		WHERE ($1::uuid IS NULL OR user_id = $1) AND ($2::uuid IS NULL OR restaurant_id = $2)
		  AND ($3 = '' OR status = $3)
		  AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5))
		ORDER BY created_at DESC, id DESC LIMIT $6`, userID, restaurantID, status, nullableTime(position.CreatedAt), nullableUUID(position.ID), limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()
	items := make([]model.Order, 0, limit)
	for rows.Next() {
		var order model.Order
		if err := rows.Scan(&order.ID, &order.UserID, &order.RestaurantID, &order.RestaurantNameSnapshot, &order.Status, &order.TotalPriceMinor, &order.Currency, &order.CreatedAt, &order.UpdatedAt); err != nil {
			return nil, "", fmt.Errorf("scan order: %w", err)
		}
		items = append(items, order)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		last := items[limit-1]
		encoded, _ := json.Marshal(orderCursor{last.CreatedAt, last.ID})
		next = base64.RawURLEncoding.EncodeToString(encoded)
		items = items[:limit]
	}
	return items, next, nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func nullableUUID(value uuid.UUID) any {
	if value == uuid.Nil {
		return nil
	}
	return value
}

func (s *Service) Cancel(ctx context.Context, orderID, userID uuid.UUID) (model.Order, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return model.Order{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock in the same outbox -> order order as the worker claim/completion paths.
	// This prevents a cancellation/claim deadlock while keeping the decision atomic.
	var eventStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'order.created' FOR UPDATE`, orderID).Scan(&eventStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Order{}, serviceerror.New("NOT_FOUND", "order not found")
	}
	if err != nil {
		return model.Order{}, fmt.Errorf("lock outbox event: %w", err)
	}
	var status string
	var dispatchStartedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT status, dispatch_started_at FROM orders WHERE id = $1 AND user_id = $2 FOR UPDATE`, orderID, userID).Scan(&status, &dispatchStartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Order{}, serviceerror.New("NOT_FOUND", "order not found")
	}
	if err != nil {
		return model.Order{}, fmt.Errorf("lock order: %w", err)
	}
	if status != PendingConfirmation || dispatchStartedAt != nil || eventStatus != "pending" {
		return model.Order{}, serviceerror.New("ORDER_NOT_CANCELLABLE", "order dispatch has started or order is no longer pending")
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status = 'cancelled', updated_at = now() WHERE id = $1`, orderID); err != nil {
		return model.Order{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox_events SET status = 'cancelled', updated_at = now() WHERE aggregate_id = $1 AND status = 'pending'`, orderID); err != nil {
		return model.Order{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_status_history (order_id, status, actor) VALUES ($1, 'cancelled', 'user')`, orderID); err != nil {
		return model.Order{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Order{}, err
	}
	return s.Get(ctx, orderID, userID)
}

func (s *Service) ApplyTransition(ctx context.Context, orderID, restaurantID uuid.UUID, next, actor string, reason *string) (model.Order, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return model.Order{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current string
	err = tx.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1 AND restaurant_id = $2 FOR UPDATE`, orderID, restaurantID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Order{}, serviceerror.New("NOT_FOUND", "order not found")
	}
	if err != nil {
		return model.Order{}, err
	}
	if current == next {
		order, loadErr := loadOrder(ctx, tx, orderID, uuid.Nil, &restaurantID)
		if loadErr != nil {
			return model.Order{}, loadErr
		}
		return order, tx.Commit(ctx)
	}
	if err := ValidateTransition(current, next); err != nil {
		return model.Order{}, serviceerror.New("INVALID_STATUS_TRANSITION", "order status transition is not allowed")
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2, updated_at = now() WHERE id = $1`, orderID, next); err != nil {
		return model.Order{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_status_history (order_id, status, actor, reason) VALUES ($1, $2, $3, $4)`, orderID, next, actor, reason); err != nil {
		return model.Order{}, err
	}
	order, err := loadOrder(ctx, tx, orderID, uuid.Nil, &restaurantID)
	if err != nil {
		return model.Order{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Order{}, err
	}
	return order, nil
}
