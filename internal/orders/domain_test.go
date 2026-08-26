package orders

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCanTransition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		from string
		to   string
		want bool
	}{
		{"accept pending", PendingConfirmation, Accepted, true},
		{"reject pending", PendingConfirmation, Rejected, true},
		{"cancel pending", PendingConfirmation, Cancelled, true},
		{"prepare accepted", Accepted, Preparing, true},
		{"skip state", Accepted, Ready, false},
		{"terminal immutable", Delivered, Cancelled, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := CanTransition(test.from, test.to); got != test.want {
				t.Fatalf("CanTransition(%q, %q) = %v, want %v", test.from, test.to, got, test.want)
			}
		})
	}
}

func TestBuildOrderItems(t *testing.T) {
	t.Parallel()
	restaurantID := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	otherRestaurantID := uuid.MustParse("10000000-0000-4000-8000-000000000002")
	itemID := uuid.MustParse("20000000-0000-4000-8000-000000000001")
	request := []RequestedItem{{MenuItemID: itemID, Quantity: 2, SeenUnitPriceMinor: 15000}}
	base := CatalogItem{ID: itemID, RestaurantID: restaurantID, ExternalItemID: "dish", Name: "Dish", UnitPriceMinor: 15000, Currency: "RUB", Available: true}
	tests := []struct {
		name      string
		catalog   []CatalogItem
		wantCode  string
		wantTotal int64
	}{
		{name: "success", catalog: []CatalogItem{base}, wantTotal: 30000},
		{name: "unavailable", catalog: []CatalogItem{withAvailable(base, false)}, wantCode: "ITEM_UNAVAILABLE"},
		{name: "price changed", catalog: []CatalogItem{withPrice(base, 16000)}, wantCode: "PRICE_CHANGED"},
		{name: "another restaurant", catalog: []CatalogItem{withRestaurant(base, otherRestaurantID)}, wantCode: "MULTIPLE_RESTAURANTS"},
		{name: "unknown item", catalog: nil, wantCode: "NOT_FOUND"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			items, total, err := BuildOrderItems(restaurantID, request, test.catalog)
			if test.wantCode == "" {
				if err != nil || len(items) != 1 || total != test.wantTotal {
					t.Fatalf("result=%+v total=%d err=%v", items, total, err)
				}
				return
			}
			var ruleErr *RuleError
			if !errors.As(err, &ruleErr) || ruleErr.Code != test.wantCode {
				t.Fatalf("error=%v, want code %s", err, test.wantCode)
			}
		})
	}
}

func withAvailable(item CatalogItem, available bool) CatalogItem {
	item.Available = available
	return item
}
func withPrice(item CatalogItem, price int64) CatalogItem       { item.UnitPriceMinor = price; return item }
func withRestaurant(item CatalogItem, id uuid.UUID) CatalogItem { item.RestaurantID = id; return item }

func TestRequestHashIgnoresItemOrder(t *testing.T) {
	t.Parallel()
	restaurantID := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	first := RequestedItem{uuid.MustParse("20000000-0000-4000-8000-000000000001"), 1, 100}
	second := RequestedItem{uuid.MustParse("20000000-0000-4000-8000-000000000002"), 2, 200}
	left, err := RequestHash(restaurantID, []RequestedItem{first, second})
	if err != nil {
		t.Fatal(err)
	}
	right, err := RequestHash(restaurantID, []RequestedItem{second, first})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatal("hash depends on item order")
	}
}
