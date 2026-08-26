// Package orders implements order use cases and state rules.
package orders

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/google/uuid"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/model"
)

const (
	PendingConfirmation = "pending_confirmation"
	Accepted            = "accepted"
	Preparing           = "preparing"
	Ready               = "ready"
	Delivering          = "delivering"
	Delivered           = "delivered"
	Rejected            = "rejected"
	Cancelled           = "cancelled"
)

var ErrInvalidTransition = errors.New("invalid order status transition")

func CanTransition(from, to string) bool {
	switch from {
	case PendingConfirmation:
		return to == Accepted || to == Rejected || to == Cancelled
	case Accepted:
		return to == Preparing
	case Preparing:
		return to == Ready
	case Ready:
		return to == Delivering
	case Delivering:
		return to == Delivered
	default:
		return false
	}
}

func ValidateTransition(from, to string) error {
	if !CanTransition(from, to) {
		return ErrInvalidTransition
	}
	return nil
}

type RequestedItem struct {
	MenuItemID         uuid.UUID `json:"menu_item_id"`
	Quantity           int32     `json:"quantity"`
	SeenUnitPriceMinor int64     `json:"seen_unit_price_minor"`
}

type CatalogItem struct {
	ID             uuid.UUID
	RestaurantID   uuid.UUID
	ExternalItemID string
	Name           string
	UnitPriceMinor int64
	Currency       string
	Available      bool
}

type RuleError struct {
	Code                  string
	MenuItemID            uuid.UUID
	CurrentUnitPriceMinor int64
}

func (e *RuleError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.MenuItemID) }

func BuildOrderItems(restaurantID uuid.UUID, requested []RequestedItem, catalog []CatalogItem) ([]model.OrderItem, int64, error) {
	requestedByID := make(map[uuid.UUID]RequestedItem, len(requested))
	for _, item := range requested {
		requestedByID[item.MenuItemID] = item
	}
	result := make([]model.OrderItem, 0, len(requested))
	var total int64
	for _, item := range catalog {
		request, wanted := requestedByID[item.ID]
		if !wanted {
			continue
		}
		if item.RestaurantID != restaurantID {
			return nil, 0, &RuleError{Code: "MULTIPLE_RESTAURANTS", MenuItemID: item.ID}
		}
		if !item.Available {
			return nil, 0, &RuleError{Code: "ITEM_UNAVAILABLE", MenuItemID: item.ID}
		}
		if item.UnitPriceMinor != request.SeenUnitPriceMinor {
			return nil, 0, &RuleError{Code: "PRICE_CHANGED", MenuItemID: item.ID, CurrentUnitPriceMinor: item.UnitPriceMinor}
		}
		if request.Quantity <= 0 || item.UnitPriceMinor <= 0 || item.UnitPriceMinor > math.MaxInt64/int64(request.Quantity) || total > math.MaxInt64-item.UnitPriceMinor*int64(request.Quantity) {
			return nil, 0, &RuleError{Code: "VALIDATION_ERROR", MenuItemID: item.ID}
		}
		lineTotal := item.UnitPriceMinor * int64(request.Quantity)
		total += lineTotal
		result = append(result, model.OrderItem{
			MenuItemID: item.ID, ExternalItemID: item.ExternalItemID, NameSnapshot: item.Name,
			UnitPriceMinor: item.UnitPriceMinor, Quantity: request.Quantity, LineTotalMinor: lineTotal, Currency: item.Currency,
		})
	}
	if len(result) != len(requested) {
		return nil, 0, &RuleError{Code: "NOT_FOUND"}
	}
	return result, total, nil
}

func RequestHash(restaurantID uuid.UUID, items []RequestedItem) ([32]byte, error) {
	normalized := append([]RequestedItem(nil), items...)
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].MenuItemID.String() < normalized[j].MenuItemID.String()
	})
	payload := struct {
		RestaurantID uuid.UUID       `json:"restaurant_id"`
		Items        []RequestedItem `json:"items"`
	}{restaurantID, normalized}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}
