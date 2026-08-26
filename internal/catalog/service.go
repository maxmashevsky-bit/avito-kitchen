// Package catalog provides public restaurant and menu reads.
package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/model"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/serviceerror"
)

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

type restaurantCursor struct {
	Name string    `json:"name"`
	ID   uuid.UUID `json:"id"`
}

func (s *Service) ListRestaurants(ctx context.Context, limit int, cursor string) ([]model.Restaurant, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", serviceerror.New("VALIDATION_ERROR", "limit must be between 1 and 100")
	}
	position := restaurantCursor{}
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(decoded, &position) != nil {
			return nil, "", serviceerror.New("VALIDATION_ERROR", "invalid cursor")
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, description, address, is_accepting_orders, created_at, updated_at
		FROM restaurants
		WHERE ($1 = '' OR (name, id) > ($1, $2))
		ORDER BY name, id
		LIMIT $3`, position.Name, position.ID, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list restaurants: %w", err)
	}
	defer rows.Close()
	items := make([]model.Restaurant, 0, limit)
	for rows.Next() {
		var item model.Restaurant
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.Address, &item.IsAcceptingOrders, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, "", fmt.Errorf("scan restaurant: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate restaurants: %w", err)
	}
	next := ""
	if len(items) > limit {
		last := items[limit-1]
		encoded, _ := json.Marshal(restaurantCursor{Name: last.Name, ID: last.ID})
		next = base64.RawURLEncoding.EncodeToString(encoded)
		items = items[:limit]
	}
	return items, next, nil
}

func (s *Service) GetRestaurant(ctx context.Context, id uuid.UUID) (model.Restaurant, error) {
	var item model.Restaurant
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, description, address, is_accepting_orders, created_at, updated_at
		FROM restaurants WHERE id = $1`, id).
		Scan(&item.ID, &item.Name, &item.Description, &item.Address, &item.IsAcceptingOrders, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Restaurant{}, serviceerror.New("NOT_FOUND", "restaurant not found")
	}
	if err != nil {
		return model.Restaurant{}, fmt.Errorf("get restaurant: %w", err)
	}
	return item, nil
}

func (s *Service) GetMenu(ctx context.Context, restaurantID uuid.UUID) (model.Menu, error) {
	var menu model.Menu
	err := s.pool.QueryRow(ctx, `
		SELECT restaurant_id, version, currency, published_at
		FROM menus WHERE restaurant_id = $1`, restaurantID).
		Scan(&menu.RestaurantID, &menu.Version, &menu.Currency, &menu.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Menu{}, serviceerror.New("NOT_FOUND", "published menu not found")
	}
	if err != nil {
		return model.Menu{}, fmt.Errorf("get menu: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, restaurant_id, external_item_id, name, description, unit_price_minor, currency, available
		FROM menu_items WHERE restaurant_id = $1 ORDER BY name, id`, restaurantID)
	if err != nil {
		return model.Menu{}, fmt.Errorf("list menu items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item model.MenuItem
		if err := rows.Scan(&item.ID, &item.RestaurantID, &item.ExternalItemID, &item.Name, &item.Description, &item.UnitPriceMinor, &item.Currency, &item.Available); err != nil {
			return model.Menu{}, fmt.Errorf("scan menu item: %w", err)
		}
		menu.Items = append(menu.Items, item)
	}
	return menu, rows.Err()
}
