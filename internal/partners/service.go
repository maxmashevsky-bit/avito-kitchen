// Package partners implements Partner API authentication, menu publishing and order updates.
package partners

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/model"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/serviceerror"
)

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

type PublishItem struct {
	ExternalItemID string `json:"external_item_id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	Available      bool   `json:"available"`
}

type Publication struct {
	RestaurantID uuid.UUID
	Version      int64
	ItemCount    int32
	PublishedAt  time.Time
	Replayed     bool
}

func (s *Service) Authenticate(ctx context.Context, token string) (uuid.UUID, error) {
	hash := sha256.Sum256([]byte(token))
	var restaurantID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT restaurant_id FROM restaurant_integrations WHERE partner_api_key_hash = $1`, hash[:]).Scan(&restaurantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, serviceerror.New("UNAUTHORIZED", "invalid Partner API key")
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("authenticate partner: %w", err)
	}
	return restaurantID, nil
}

func (s *Service) PublishMenu(ctx context.Context, restaurantID uuid.UUID, version int64, currency string, items []PublishItem) (Publication, error) {
	if version < 1 || currency != "RUB" || len(items) == 0 {
		return Publication{}, serviceerror.New("VALIDATION_ERROR", "version, RUB currency and at least one item are required")
	}
	normalized := append([]PublishItem(nil), items...)
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ExternalItemID < normalized[j].ExternalItemID })
	for i, item := range normalized {
		if strings.TrimSpace(item.ExternalItemID) == "" || strings.TrimSpace(item.Name) == "" || item.UnitPriceMinor <= 0 {
			return Publication{}, serviceerror.New("VALIDATION_ERROR", "menu items require non-empty IDs/names and positive prices")
		}
		if i > 0 && normalized[i-1].ExternalItemID == item.ExternalItemID {
			return Publication{}, serviceerror.New("VALIDATION_ERROR", "external_item_id must be unique within a menu")
		}
	}
	hash, err := menuPayloadHash(currency, normalized)
	if err != nil {
		return Publication{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin menu publication: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, restaurantID.String()); err != nil {
		return Publication{}, fmt.Errorf("lock restaurant menu: %w", err)
	}

	var currentVersion int64
	var currentHash []byte
	var publishedAt time.Time
	err = tx.QueryRow(ctx, `SELECT version, payload_hash, published_at FROM menus WHERE restaurant_id = $1 FOR UPDATE`, restaurantID).
		Scan(&currentVersion, &currentHash, &publishedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, fmt.Errorf("lock menu: %w", err)
	}
	if err == nil && version <= currentVersion {
		if version == currentVersion && string(currentHash) == string(hash[:]) {
			return Publication{restaurantID, version, int32(len(items)), publishedAt, true}, tx.Commit(ctx)
		}
		return Publication{}, serviceerror.New("STALE_MENU_VERSION", "menu version is stale or already used with different content")
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO menus (restaurant_id, version, payload_hash, currency, published_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (restaurant_id) DO UPDATE SET
			version = EXCLUDED.version, payload_hash = EXCLUDED.payload_hash, currency = EXCLUDED.currency,
			published_at = now(), updated_at = now()
		RETURNING published_at`, restaurantID, version, hash[:], currency).Scan(&publishedAt)
	if err != nil {
		return Publication{}, fmt.Errorf("upsert menu: %w", err)
	}
	ids := make([]string, 0, len(normalized))
	for _, item := range normalized {
		ids = append(ids, item.ExternalItemID)
		_, err = tx.Exec(ctx, `
			INSERT INTO menu_items (restaurant_id, external_item_id, name, description, unit_price_minor, currency, available)
			VALUES ($1, $2, $3, $4, $5, 'RUB', $6)
			ON CONFLICT (restaurant_id, external_item_id) DO UPDATE SET
				name = EXCLUDED.name, description = EXCLUDED.description,
				unit_price_minor = EXCLUDED.unit_price_minor, available = EXCLUDED.available, updated_at = now()`,
			restaurantID, item.ExternalItemID, item.Name, item.Description, item.UnitPriceMinor, item.Available)
		if err != nil {
			return Publication{}, fmt.Errorf("upsert menu item: %w", err)
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM menu_items WHERE restaurant_id = $1 AND NOT (external_item_id = ANY($2))`, restaurantID, ids)
	if err != nil {
		return Publication{}, fmt.Errorf("delete removed menu items: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit menu publication: %w", err)
	}
	return Publication{restaurantID, version, int32(len(items)), publishedAt, false}, nil
}

func (s *Service) PatchMenuItem(ctx context.Context, restaurantID uuid.UUID, externalID string, expectedVersion int64, patch PublishItemPatch) (model.MenuItem, int64, error) {
	if expectedVersion < 1 || strings.TrimSpace(externalID) == "" {
		return model.MenuItem{}, 0, serviceerror.New("VALIDATION_ERROR", "expected_version and external item ID are required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return model.MenuItem{}, 0, fmt.Errorf("begin menu patch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var currentVersion int64
	if err := tx.QueryRow(ctx, `SELECT version FROM menus WHERE restaurant_id = $1 FOR UPDATE`, restaurantID).Scan(&currentVersion); errors.Is(err, pgx.ErrNoRows) {
		return model.MenuItem{}, 0, serviceerror.New("NOT_FOUND", "published menu not found")
	} else if err != nil {
		return model.MenuItem{}, 0, fmt.Errorf("lock menu: %w", err)
	}
	if currentVersion != expectedVersion {
		return model.MenuItem{}, 0, serviceerror.New("STALE_MENU_VERSION", "expected menu version does not match")
	}
	var item model.MenuItem
	err = tx.QueryRow(ctx, `
		UPDATE menu_items SET
			name = COALESCE($3, name), description = COALESCE($4, description),
			unit_price_minor = COALESCE($5, unit_price_minor), available = COALESCE($6, available), updated_at = now()
		WHERE restaurant_id = $1 AND external_item_id = $2
		RETURNING id, restaurant_id, external_item_id, name, description, unit_price_minor, currency, available`,
		restaurantID, externalID, patch.Name, patch.Description, patch.UnitPriceMinor, patch.Available).
		Scan(&item.ID, &item.RestaurantID, &item.ExternalItemID, &item.Name, &item.Description, &item.UnitPriceMinor, &item.Currency, &item.Available)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.MenuItem{}, 0, serviceerror.New("NOT_FOUND", "menu item not found")
	}
	if err != nil {
		return model.MenuItem{}, 0, fmt.Errorf("patch menu item: %w", err)
	}
	if item.UnitPriceMinor <= 0 || strings.TrimSpace(item.Name) == "" {
		return model.MenuItem{}, 0, serviceerror.New("VALIDATION_ERROR", "name and price must remain valid")
	}
	newVersion := currentVersion + 1
	rows, err := tx.Query(ctx, `
		SELECT external_item_id, name, description, unit_price_minor, available
		FROM menu_items WHERE restaurant_id = $1 ORDER BY external_item_id`, restaurantID)
	if err != nil {
		return model.MenuItem{}, 0, fmt.Errorf("read patched menu: %w", err)
	}
	var currentItems []PublishItem
	for rows.Next() {
		var current PublishItem
		if err := rows.Scan(&current.ExternalItemID, &current.Name, &current.Description, &current.UnitPriceMinor, &current.Available); err != nil {
			rows.Close()
			return model.MenuItem{}, 0, fmt.Errorf("scan patched menu: %w", err)
		}
		currentItems = append(currentItems, current)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return model.MenuItem{}, 0, fmt.Errorf("iterate patched menu: %w", err)
	}
	hash, err := menuPayloadHash("RUB", currentItems)
	if err != nil {
		return model.MenuItem{}, 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE menus SET version = $2, payload_hash = $3, published_at = now(), updated_at = now() WHERE restaurant_id = $1`, restaurantID, newVersion, hash[:]); err != nil {
		return model.MenuItem{}, 0, fmt.Errorf("advance menu version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.MenuItem{}, 0, fmt.Errorf("commit menu patch: %w", err)
	}
	return item, newVersion, nil
}

type PublishItemPatch struct {
	Name           *string
	Description    *string
	UnitPriceMinor *int64
	Available      *bool
}

func menuPayloadHash(currency string, items []PublishItem) ([32]byte, error) {
	payload, err := json.Marshal(struct {
		Currency string        `json:"currency"`
		Items    []PublishItem `json:"items"`
	}{currency, items})
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode menu hash: %w", err)
	}
	return sha256.Sum256(payload), nil
}
