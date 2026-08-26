// Package kitchenhttp adapts kitchen use cases to the generated OpenAPI server interface.
package kitchenhttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/catalog"
	kitchenapi "github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/generated/kitchenapi"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/model"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/orders"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/partners"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/httpx"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/serviceerror"
)

type Handler struct {
	pool     *pgxpool.Pool
	catalog  *catalog.Service
	orders   *orders.Service
	partners *partners.Service
	maxBody  int64
}

func New(pool *pgxpool.Pool, maxBody int64) *Handler {
	return &Handler{pool: pool, catalog: catalog.New(pool), orders: orders.New(pool), partners: partners.New(pool), maxBody: maxBody}
}

func Router(handler *Handler, logger *slog.Logger) http.Handler {
	router := chi.NewRouter()
	router.Use(httpx.Middleware(logger))
	return kitchenapi.HandlerWithOptions(handler, kitchenapi.ChiServerOptions{
		BaseRouter: router,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
			httpx.WriteError(w, r, httpx.NewError(http.StatusBadRequest, "VALIDATION_ERROR", "invalid path, query or header parameter"))
		},
	})
}

func (h *Handler) GetHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, kitchenapi.HealthResponse{Status: kitchenapi.HealthResponseStatusOk})
}

func (h *Handler) GetReadiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := h.pool.Ping(ctx); err != nil {
		httpx.WriteError(w, r, httpx.NewError(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "database is not ready"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, kitchenapi.HealthResponse{Status: kitchenapi.HealthResponseStatusReady})
}

func (h *Handler) ListRestaurants(w http.ResponseWriter, r *http.Request, params kitchenapi.ListRestaurantsParams) {
	limit := 20
	if params.Limit != nil {
		limit = int(*params.Limit)
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	items, next, err := h.catalog.ListRestaurants(r.Context(), limit, cursor)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	response := kitchenapi.RestaurantPage{Items: make([]kitchenapi.RestaurantSummary, 0, len(items))}
	for _, item := range items {
		response.Items = append(response.Items, kitchenapi.RestaurantSummary{Id: item.ID, Name: item.Name, Description: optional(item.Description), IsAcceptingOrders: item.IsAcceptingOrders})
	}
	if next != "" {
		response.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, response)
}

func (h *Handler) GetRestaurant(w http.ResponseWriter, r *http.Request, restaurantID kitchenapi.RestaurantID) {
	item, err := h.catalog.GetRestaurant(r.Context(), restaurantID)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, kitchenapi.Restaurant{Id: item.ID, Name: item.Name, Description: optional(item.Description), Address: optional(item.Address), IsAcceptingOrders: item.IsAcceptingOrders, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt})
}

func (h *Handler) GetRestaurantMenu(w http.ResponseWriter, r *http.Request, restaurantID kitchenapi.RestaurantID) {
	menu, err := h.catalog.GetMenu(r.Context(), restaurantID)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	response := kitchenapi.Menu{RestaurantId: menu.RestaurantID, Version: menu.Version, Currency: kitchenapi.Currency(menu.Currency), PublishedAt: menu.PublishedAt, Items: make([]kitchenapi.MenuItem, 0, len(menu.Items))}
	for _, item := range menu.Items {
		response.Items = append(response.Items, menuItem(item))
	}
	httpx.WriteJSON(w, http.StatusOK, response)
}

func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request, params kitchenapi.CreateOrderParams) {
	var request kitchenapi.CreateOrderRequest
	if err := httpx.DecodeJSON(w, r, h.maxBody, &request); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]orders.RequestedItem, 0, len(request.Items))
	for _, item := range request.Items {
		items = append(items, orders.RequestedItem{MenuItemID: item.MenuItemId, Quantity: item.Quantity, SeenUnitPriceMinor: item.SeenUnitPriceMinor})
	}
	result, err := h.orders.Create(r.Context(), params.XUserID, request.RestaurantId, params.IdempotencyKey, items)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	w.Header().Set("Idempotency-Replayed", boolString(result.Replayed))
	httpx.WriteJSON(w, status, orderResponse(result.Order))
}

func (h *Handler) ListUserOrders(w http.ResponseWriter, r *http.Request, params kitchenapi.ListUserOrdersParams) {
	limit := 20
	if params.Limit != nil {
		limit = int(*params.Limit)
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	items, next, err := h.orders.ListUser(r.Context(), params.XUserID, limit, cursor)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	response := kitchenapi.OrderPage{Items: make([]kitchenapi.OrderSummary, 0, len(items))}
	for _, item := range items {
		response.Items = append(response.Items, orderSummary(item))
	}
	if next != "" {
		response.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, response)
}

func (h *Handler) GetUserOrder(w http.ResponseWriter, r *http.Request, orderID kitchenapi.OrderID, params kitchenapi.GetUserOrderParams) {
	item, err := h.orders.Get(r.Context(), orderID, params.XUserID)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, orderResponse(item))
}

func (h *Handler) CancelUserOrder(w http.ResponseWriter, r *http.Request, orderID kitchenapi.OrderID, params kitchenapi.CancelUserOrderParams) {
	item, err := h.orders.Cancel(r.Context(), orderID, params.XUserID)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, orderResponse(item))
}

func (h *Handler) PublishPartnerMenu(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := h.partnerRestaurant(w, r)
	if !ok {
		return
	}
	var request kitchenapi.PublishMenuRequest
	if err := httpx.DecodeJSON(w, r, h.maxBody, &request); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]partners.PublishItem, 0, len(request.Items))
	for _, item := range request.Items {
		description := ""
		if item.Description != nil {
			description = *item.Description
		}
		items = append(items, partners.PublishItem{ExternalItemID: item.ExternalItemId, Name: item.Name, Description: description, UnitPriceMinor: item.UnitPriceMinor, Available: item.Available})
	}
	publication, err := h.partners.PublishMenu(r.Context(), restaurantID, request.Version, string(request.Currency), items)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	status := http.StatusCreated
	if publication.Replayed {
		status = http.StatusOK
	}
	w.Header().Set("Idempotency-Replayed", boolString(publication.Replayed))
	httpx.WriteJSON(w, status, kitchenapi.MenuPublication{RestaurantId: publication.RestaurantID, Version: publication.Version, ItemCount: publication.ItemCount, PublishedAt: publication.PublishedAt})
}

func (h *Handler) PatchPartnerMenuItem(w http.ResponseWriter, r *http.Request, externalID kitchenapi.ExternalItemID) {
	restaurantID, ok := h.partnerRestaurant(w, r)
	if !ok {
		return
	}
	var request kitchenapi.PatchMenuItemRequest
	if err := httpx.DecodeJSON(w, r, h.maxBody, &request); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	item, version, err := h.partners.PatchMenuItem(r.Context(), restaurantID, externalID, request.ExpectedVersion, partners.PublishItemPatch{Name: request.Name, Description: request.Description, UnitPriceMinor: request.UnitPriceMinor, Available: request.Available})
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, kitchenapi.MenuItemMutation{RestaurantId: restaurantID, MenuVersion: version, Item: menuItem(item)})
}

func (h *Handler) ListPartnerOrders(w http.ResponseWriter, r *http.Request, params kitchenapi.ListPartnerOrdersParams) {
	restaurantID, ok := h.partnerRestaurant(w, r)
	if !ok {
		return
	}
	limit := 20
	if params.Limit != nil {
		limit = int(*params.Limit)
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	status := ""
	if params.Status != nil {
		status = string(*params.Status)
	}
	items, next, err := h.orders.ListPartner(r.Context(), restaurantID, status, limit, cursor)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	response := kitchenapi.PartnerOrderPage{Items: make([]kitchenapi.PartnerOrder, 0, len(items))}
	for _, summary := range items {
		full, loadErr := h.orders.GetPartner(r.Context(), summary.ID, restaurantID)
		if loadErr != nil {
			writeServiceError(w, r, loadErr)
			return
		}
		response.Items = append(response.Items, partnerOrder(full))
	}
	if next != "" {
		response.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, response)
}

func (h *Handler) DecidePartnerOrder(w http.ResponseWriter, r *http.Request, orderID kitchenapi.OrderID) {
	restaurantID, ok := h.partnerRestaurant(w, r)
	if !ok {
		return
	}
	var request kitchenapi.OrderDecisionRequest
	if err := httpx.DecodeJSON(w, r, h.maxBody, &request); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	next := string(request.Decision)
	if next == orders.Rejected && (request.Reason == nil || *request.Reason == "") {
		writeServiceError(w, r, serviceerror.New("VALIDATION_ERROR", "rejection reason is required"))
		return
	}
	item, err := h.orders.ApplyTransition(r.Context(), orderID, restaurantID, next, "restaurant", request.Reason)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, partnerOrder(item))
}

func (h *Handler) UpdatePartnerOrderStatus(w http.ResponseWriter, r *http.Request, orderID kitchenapi.OrderID) {
	restaurantID, ok := h.partnerRestaurant(w, r)
	if !ok {
		return
	}
	var request kitchenapi.OrderStatusUpdateRequest
	if err := httpx.DecodeJSON(w, r, h.maxBody, &request); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	item, err := h.orders.ApplyTransition(r.Context(), orderID, restaurantID, string(request.Status), "restaurant", nil)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, partnerOrder(item))
}

func (h *Handler) partnerRestaurant(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	token, ok := httpx.BearerToken(r)
	if !ok {
		writeServiceError(w, r, serviceerror.New("UNAUTHORIZED", "Partner Bearer key is required"))
		return uuid.Nil, false
	}
	id, err := h.partners.Authenticate(r.Context(), token)
	if err != nil {
		writeServiceError(w, r, err)
		return uuid.Nil, false
	}
	return id, true
}

func menuItem(item model.MenuItem) kitchenapi.MenuItem {
	return kitchenapi.MenuItem{Id: item.ID, ExternalItemId: item.ExternalItemID, Name: item.Name, Description: optional(item.Description), UnitPriceMinor: item.UnitPriceMinor, Currency: kitchenapi.Currency(item.Currency), Available: item.Available}
}

func orderSummary(item model.Order) kitchenapi.OrderSummary {
	return kitchenapi.OrderSummary{Id: item.ID, RestaurantId: item.RestaurantID, RestaurantName: item.RestaurantNameSnapshot, Status: kitchenapi.OrderStatus(item.Status), TotalPriceMinor: item.TotalPriceMinor, Currency: kitchenapi.Currency(item.Currency), CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
}

func orderResponse(item model.Order) kitchenapi.Order {
	response := kitchenapi.Order{Id: item.ID, RestaurantId: item.RestaurantID, RestaurantName: item.RestaurantNameSnapshot, Status: kitchenapi.OrderStatus(item.Status), TotalPriceMinor: item.TotalPriceMinor, Currency: kitchenapi.Currency(item.Currency), CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, Items: make([]kitchenapi.OrderItem, 0, len(item.Items)), StatusHistory: make([]kitchenapi.OrderStatusHistoryEntry, 0, len(item.History))}
	for _, value := range item.Items {
		response.Items = append(response.Items, kitchenapi.OrderItem{MenuItemId: value.MenuItemID, ExternalItemId: value.ExternalItemID, Name: value.NameSnapshot, UnitPriceMinor: value.UnitPriceMinor, Quantity: value.Quantity, LineTotalMinor: value.LineTotalMinor, Currency: kitchenapi.Currency(value.Currency)})
	}
	for _, value := range item.History {
		actor := value.Actor
		if actor == "worker" {
			actor = "platform"
		}
		response.StatusHistory = append(response.StatusHistory, kitchenapi.OrderStatusHistoryEntry{Status: kitchenapi.OrderStatus(value.Status), Actor: kitchenapi.OrderStatusHistoryEntryActor(actor), Reason: value.Reason, ChangedAt: value.ChangedAt})
	}
	return response
}

func partnerOrder(item model.Order) kitchenapi.PartnerOrder {
	response := kitchenapi.PartnerOrder{Id: item.ID, RestaurantId: item.RestaurantID, Status: kitchenapi.OrderStatus(item.Status), TotalPriceMinor: item.TotalPriceMinor, Currency: kitchenapi.Currency(item.Currency), CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, Items: make([]kitchenapi.PartnerOrderItem, 0, len(item.Items))}
	for _, value := range item.Items {
		response.Items = append(response.Items, kitchenapi.PartnerOrderItem{ExternalItemId: value.ExternalItemID, Name: value.NameSnapshot, UnitPriceMinor: value.UnitPriceMinor, Quantity: value.Quantity, LineTotalMinor: value.LineTotalMinor, Currency: kitchenapi.Currency(value.Currency)})
	}
	if len(item.History) > 0 {
		response.DecisionReason = item.History[len(item.History)-1].Reason
	}
	return response
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var serviceErr *serviceerror.Error
	if !errors.As(err, &serviceErr) {
		httpx.WriteError(w, r, err)
		return
	}
	status := http.StatusInternalServerError
	switch serviceErr.Code {
	case "VALIDATION_ERROR":
		status = http.StatusBadRequest
	case "UNAUTHORIZED":
		status = http.StatusUnauthorized
	case "NOT_FOUND":
		status = http.StatusNotFound
	case "MULTIPLE_RESTAURANTS":
		status = http.StatusUnprocessableEntity
	case "IDEMPOTENCY_KEY_REUSED", "ITEM_UNAVAILABLE", "PRICE_CHANGED", "RESTAURANT_CLOSED", "STALE_MENU_VERSION", "ORDER_NOT_CANCELLABLE", "INVALID_STATUS_TRANSITION":
		status = http.StatusConflict
	}
	httpx.WriteError(w, r, &httpx.Error{Status: status, Code: serviceErr.Code, Message: serviceErr.Message, Details: serviceErr.Details})
}
