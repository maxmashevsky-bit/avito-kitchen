// Package model defines domain data shared by use cases, without HTTP or PostgreSQL dependencies.
package model

import (
	"time"

	"github.com/google/uuid"
)

type Restaurant struct {
	ID                uuid.UUID
	Name              string
	Description       string
	Address           string
	IsAcceptingOrders bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type Menu struct {
	RestaurantID uuid.UUID
	Version      int64
	Currency     string
	PublishedAt  time.Time
	Items        []MenuItem
}

type MenuItem struct {
	ID             uuid.UUID
	RestaurantID   uuid.UUID
	ExternalItemID string
	Name           string
	Description    string
	UnitPriceMinor int64
	Currency       string
	Available      bool
}

type Order struct {
	ID                     uuid.UUID
	UserID                 uuid.UUID
	RestaurantID           uuid.UUID
	RestaurantNameSnapshot string
	Status                 string
	TotalPriceMinor        int64
	Currency               string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	Items                  []OrderItem
	History                []OrderHistory
}

type OrderItem struct {
	MenuItemID     uuid.UUID
	ExternalItemID string
	NameSnapshot   string
	UnitPriceMinor int64
	Quantity       int32
	LineTotalMinor int64
	Currency       string
}

type OrderHistory struct {
	Status    string
	Actor     string
	Reason    *string
	ChangedAt time.Time
}
