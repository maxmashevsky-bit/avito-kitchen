// Package restaurantdemo owns demo-restaurant stock and incoming order decisions.
package restaurantdemo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/serviceerror"
)

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

type IncomingItem struct {
	PlatformMenuItemID uuid.UUID `json:"platform_menu_item_id"`
	ExternalItemID     string    `json:"external_item_id"`
	Name               string    `json:"name"`
	UnitPriceMinor     int64     `json:"unit_price_minor"`
	Quantity           int32     `json:"quantity"`
	LineTotalMinor     int64     `json:"line_total_minor"`
	Currency           string    `json:"currency"`
}

type IncomingRequest struct {
	PlatformOrderID      uuid.UUID      `json:"platform_order_id"`
	PlatformRestaurantID uuid.UUID      `json:"platform_restaurant_id"`
	CreatedAt            time.Time      `json:"created_at"`
	TotalPriceMinor      int64          `json:"total_price_minor"`
	Currency             string         `json:"currency"`
	Items                []IncomingItem `json:"items"`
}

type Decision struct {
	Request            IncomingRequest
	Status             string
	RejectionCode      *string
	RejectionMessage   *string
	UnavailableItemIDs []string
	ReceivedAt         time.Time
	DecidedAt          time.Time
	Replayed           bool
}

func (s *Service) Receive(ctx context.Context, request IncomingRequest) (Decision, error) {
	if request.PlatformOrderID == uuid.Nil || request.PlatformRestaurantID == uuid.Nil || request.Currency != "RUB" || request.TotalPriceMinor <= 0 || len(request.Items) == 0 {
		return Decision{}, serviceerror.New("VALIDATION_ERROR", "order identifiers, RUB currency, positive total and items are required")
	}
	normalized := request
	normalized.Items = append([]IncomingItem(nil), request.Items...)
	sort.Slice(normalized.Items, func(i, j int) bool { return normalized.Items[i].ExternalItemID < normalized.Items[j].ExternalItemID })
	var calculated int64
	seenExternal := make(map[string]struct{}, len(normalized.Items))
	seenPlatform := make(map[uuid.UUID]struct{}, len(normalized.Items))
	for _, item := range normalized.Items {
		if item.PlatformMenuItemID == uuid.Nil || strings.TrimSpace(item.ExternalItemID) == "" || item.Quantity <= 0 || item.UnitPriceMinor <= 0 || item.UnitPriceMinor > math.MaxInt64/int64(item.Quantity) || item.LineTotalMinor != item.UnitPriceMinor*int64(item.Quantity) || item.Currency != "RUB" {
			return Decision{}, serviceerror.New("VALIDATION_ERROR", "invalid incoming order item")
		}
		if _, duplicate := seenExternal[item.ExternalItemID]; duplicate {
			return Decision{}, serviceerror.New("VALIDATION_ERROR", "incoming item IDs must be unique")
		}
		if _, duplicate := seenPlatform[item.PlatformMenuItemID]; duplicate {
			return Decision{}, serviceerror.New("VALIDATION_ERROR", "incoming item IDs must be unique")
		}
		seenExternal[item.ExternalItemID] = struct{}{}
		seenPlatform[item.PlatformMenuItemID] = struct{}{}
		if calculated > math.MaxInt64-item.LineTotalMinor {
			return Decision{}, serviceerror.New("VALIDATION_ERROR", "incoming order total is too large")
		}
		calculated += item.LineTotalMinor
	}
	if calculated != request.TotalPriceMinor {
		return Decision{}, serviceerror.New("VALIDATION_ERROR", "incoming order total does not match item totals")
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return Decision{}, err
	}
	hash := sha256.Sum256(encoded)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Decision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serializes concurrent first delivery of the same platform ID without a process-local lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, request.PlatformOrderID.String()); err != nil {
		return Decision{}, fmt.Errorf("lock platform order ID: %w", err)
	}
	if existing, existingHash, found, err := loadDecision(ctx, tx, request.PlatformOrderID); err != nil {
		return Decision{}, err
	} else if found {
		if !bytes.Equal(existingHash, hash[:]) {
			return Decision{}, serviceerror.New("PLATFORM_ORDER_ID_REUSED", "platform_order_id was reused with different content")
		}
		existing.Replayed = true
		return existing, tx.Commit(ctx)
	}

	externalIDs := make([]string, 0, len(normalized.Items))
	for _, item := range normalized.Items {
		externalIDs = append(externalIDs, item.ExternalItemID)
	}
	rows, err := tx.Query(ctx, `SELECT external_item_id, available, available_quantity FROM items WHERE external_item_id = ANY($1) ORDER BY external_item_id FOR UPDATE`, externalIDs)
	if err != nil {
		return Decision{}, fmt.Errorf("lock stock: %w", err)
	}
	stock := make(map[string]struct {
		available bool
		quantity  int32
	}, len(externalIDs))
	for rows.Next() {
		var id string
		var available bool
		var quantity int32
		if err := rows.Scan(&id, &available, &quantity); err != nil {
			rows.Close()
			return Decision{}, err
		}
		stock[id] = struct {
			available bool
			quantity  int32
		}{available, quantity}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Decision{}, err
	}
	unavailable := make([]string, 0)
	for _, item := range normalized.Items {
		value, exists := stock[item.ExternalItemID]
		if !exists || !value.available || value.quantity < item.Quantity {
			unavailable = append(unavailable, item.ExternalItemID)
		}
	}
	status := "accepted"
	var rejectionCode, rejectionMessage *string
	if len(unavailable) > 0 {
		status = "rejected"
		code, message := "ITEM_UNAVAILABLE", "one or more items have insufficient stock"
		rejectionCode, rejectionMessage = &code, &message
	} else {
		for _, item := range normalized.Items {
			result, err := tx.Exec(ctx, `
				UPDATE items SET available_quantity = available_quantity - $2,
					reserved_quantity = reserved_quantity + $2, updated_at = now()
				WHERE external_item_id = $1 AND available AND available_quantity >= $2`, item.ExternalItemID, item.Quantity)
			if err != nil {
				return Decision{}, fmt.Errorf("reserve stock: %w", err)
			}
			if result.RowsAffected() != 1 {
				return Decision{}, errors.New("stock changed while rows were locked")
			}
		}
	}
	var receivedAt, decidedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO incoming_orders (platform_order_id, platform_restaurant_id, request_hash, status, total_price_minor, currency, rejection_code, rejection_message, rejection_external_item_ids)
		VALUES ($1, $2, $3, $4, $5, 'RUB', $6, $7, $8)
		RETURNING received_at, decided_at`, request.PlatformOrderID, request.PlatformRestaurantID, hash[:], status, request.TotalPriceMinor, rejectionCode, rejectionMessage, nullableStrings(unavailable)).
		Scan(&receivedAt, &decidedAt)
	if err != nil {
		return Decision{}, fmt.Errorf("insert incoming order: %w", err)
	}
	for _, item := range normalized.Items {
		_, err := tx.Exec(ctx, `
			INSERT INTO incoming_order_items (platform_order_id, platform_menu_item_id, external_item_id, name_snapshot, unit_price_minor, quantity, line_total_minor, currency)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, request.PlatformOrderID, item.PlatformMenuItemID, item.ExternalItemID, item.Name, item.UnitPriceMinor, item.Quantity, item.LineTotalMinor, item.Currency)
		if err != nil {
			return Decision{}, fmt.Errorf("insert incoming item: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	return Decision{Request: normalized, Status: status, RejectionCode: rejectionCode, RejectionMessage: rejectionMessage, UnavailableItemIDs: unavailable, ReceivedAt: receivedAt, DecidedAt: decidedAt}, nil
}

type decisionQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadDecision(ctx context.Context, db decisionQueryer, orderID uuid.UUID) (Decision, []byte, bool, error) {
	var result Decision
	var hash []byte
	result.Request.PlatformOrderID = orderID
	err := db.QueryRow(ctx, `
		SELECT platform_restaurant_id, request_hash, status, total_price_minor, currency, rejection_code, rejection_message,
			COALESCE(rejection_external_item_ids, ARRAY[]::text[]), received_at, decided_at
		FROM incoming_orders WHERE platform_order_id = $1`, orderID).
		Scan(&result.Request.PlatformRestaurantID, &hash, &result.Status, &result.Request.TotalPriceMinor, &result.Request.Currency, &result.RejectionCode, &result.RejectionMessage, &result.UnavailableItemIDs, &result.ReceivedAt, &result.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, nil, false, nil
	}
	if err != nil {
		return Decision{}, nil, false, fmt.Errorf("load incoming order: %w", err)
	}
	rows, err := db.Query(ctx, `SELECT platform_menu_item_id, external_item_id, name_snapshot, unit_price_minor, quantity, line_total_minor, currency FROM incoming_order_items WHERE platform_order_id = $1 ORDER BY id`, orderID)
	if err != nil {
		return Decision{}, nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var item IncomingItem
		if err := rows.Scan(&item.PlatformMenuItemID, &item.ExternalItemID, &item.Name, &item.UnitPriceMinor, &item.Quantity, &item.LineTotalMinor, &item.Currency); err != nil {
			return Decision{}, nil, false, err
		}
		result.Request.Items = append(result.Request.Items, item)
	}
	return result, hash, true, rows.Err()
}

func (s *Service) Get(ctx context.Context, orderID uuid.UUID) (Decision, error) {
	result, _, found, err := loadDecision(ctx, s.pool, orderID)
	if err != nil {
		return Decision{}, err
	}
	if !found {
		return Decision{}, serviceerror.New("NOT_FOUND", "incoming order not found")
	}
	return result, nil
}

type MenuItem struct {
	ExternalItemID string
	Name           string
	Description    string
	UnitPriceMinor int64
	Available      bool
}

func (s *Service) Menu(ctx context.Context) ([]MenuItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT external_item_id, name, description, unit_price_minor, available AND available_quantity > 0 FROM items ORDER BY external_item_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []MenuItem
	for rows.Next() {
		var item MenuItem
		if err := rows.Scan(&item.ExternalItemID, &item.Name, &item.Description, &item.UnitPriceMinor, &item.Available); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func nullableStrings(values []string) any {
	if len(values) == 0 {
		return nil
	}
	return values
}
