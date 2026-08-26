// Package restauranthttp adapts the demo restaurant to its generated integration API.
package restauranthttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	restaurantapi "github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/generated/restaurantapi"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/httpx"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/restaurantdemo"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/serviceerror"
)

type Handler struct {
	pool           *pgxpool.Pool
	service        *restaurantdemo.Service
	integrationKey string
	maxBody        int64
}

func New(pool *pgxpool.Pool, integrationKey string, maxBody int64) *Handler {
	return &Handler{pool: pool, service: restaurantdemo.New(pool), integrationKey: integrationKey, maxBody: maxBody}
}

func Router(handler *Handler, logger *slog.Logger) http.Handler {
	router := chi.NewRouter()
	router.Use(httpx.Middleware(logger))
	return restaurantapi.HandlerWithOptions(handler, restaurantapi.ChiServerOptions{
		BaseRouter: router,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
			httpx.WriteError(w, r, httpx.NewError(http.StatusBadRequest, "VALIDATION_ERROR", "invalid path parameter"))
		},
	})
}

func (h *Handler) GetRestaurantHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, restaurantapi.HealthResponse{Status: restaurantapi.Ok})
}

func (h *Handler) GetRestaurantReadiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := h.pool.Ping(ctx); err != nil {
		httpx.WriteError(w, r, httpx.NewError(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "database is not ready"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, restaurantapi.HealthResponse{Status: restaurantapi.Ready})
}

func (h *Handler) ReceivePlatformOrder(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		httpx.WriteError(w, r, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "invalid integration key"))
		return
	}
	var request restaurantapi.IncomingOrderRequest
	if err := httpx.DecodeJSON(w, r, h.maxBody, &request); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	input := restaurantdemo.IncomingRequest{PlatformOrderID: request.PlatformOrderId, PlatformRestaurantID: request.PlatformRestaurantId, CreatedAt: request.CreatedAt, TotalPriceMinor: request.TotalPriceMinor, Currency: string(request.Currency), Items: make([]restaurantdemo.IncomingItem, 0, len(request.Items))}
	for _, item := range request.Items {
		input.Items = append(input.Items, restaurantdemo.IncomingItem{PlatformMenuItemID: item.PlatformMenuItemId, ExternalItemID: item.ExternalItemId, Name: item.Name, UnitPriceMinor: item.UnitPriceMinor, Quantity: item.Quantity, LineTotalMinor: item.LineTotalMinor, Currency: string(item.Currency)})
	}
	decision, err := h.service.Receive(r.Context(), input)
	if err != nil {
		writeError(w, r, err)
		return
	}
	status := http.StatusCreated
	if decision.Replayed {
		status = http.StatusOK
	}
	w.Header().Set("Idempotency-Replayed", boolString(decision.Replayed))
	httpx.WriteJSON(w, status, decisionResponse(decision))
}

func (h *Handler) GetPlatformOrder(w http.ResponseWriter, r *http.Request, platformOrderID restaurantapi.PlatformOrderID) {
	if !h.authorized(r) {
		httpx.WriteError(w, r, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "invalid integration key"))
		return
	}
	decision, err := h.service.Get(r.Context(), platformOrderID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, decisionResponse(decision))
}

func (h *Handler) authorized(r *http.Request) bool {
	token, ok := httpx.BearerToken(r)
	return ok && httpx.SecureEqual(token, h.integrationKey)
}

func decisionResponse(value restaurantdemo.Decision) restaurantapi.IncomingOrder {
	response := restaurantapi.IncomingOrder{PlatformOrderId: value.Request.PlatformOrderID, Status: restaurantapi.IncomingOrderStatus(value.Status), TotalPriceMinor: value.Request.TotalPriceMinor, Currency: restaurantapi.Currency(value.Request.Currency), ReceivedAt: value.ReceivedAt, DecidedAt: value.DecidedAt, Items: make([]restaurantapi.IncomingOrderItem, 0, len(value.Request.Items))}
	for _, item := range value.Request.Items {
		response.Items = append(response.Items, restaurantapi.IncomingOrderItem{PlatformMenuItemId: item.PlatformMenuItemID, ExternalItemId: item.ExternalItemID, Name: item.Name, UnitPriceMinor: item.UnitPriceMinor, Quantity: item.Quantity, LineTotalMinor: item.LineTotalMinor, Currency: restaurantapi.Currency(item.Currency)})
	}
	if value.RejectionCode != nil && value.RejectionMessage != nil {
		ids := append([]string(nil), value.UnavailableItemIDs...)
		response.Rejection = &restaurantapi.Rejection{Code: restaurantapi.RejectionCode(*value.RejectionCode), Message: *value.RejectionMessage, ExternalItemIds: &ids}
	}
	return response
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
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
	case "PLATFORM_ORDER_ID_REUSED":
		status = http.StatusConflict
	}
	httpx.WriteError(w, r, &httpx.Error{Status: status, Code: serviceErr.Code, Message: serviceErr.Message, Details: serviceErr.Details})
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
