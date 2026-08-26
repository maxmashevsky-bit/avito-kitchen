package restaurantdemo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const DemoRestaurantID = "10000000-0000-4000-8000-000000000001"

func (s *Service) PublishMenuUntilSuccess(ctx context.Context, logger *slog.Logger, kitchenURL, partnerKey string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		err := s.publishMenu(ctx, kitchenURL, partnerKey)
		if err == nil {
			logger.Info("demo menu published")
			return
		}
		logger.Warn("demo menu publication delayed", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) publishMenu(ctx context.Context, kitchenURL, partnerKey string) error {
	items, err := s.Menu(ctx)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	version := int64(1)
	getRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(kitchenURL, "/")+"/api/v1/restaurants/"+DemoRestaurantID+"/menu", nil)
	if err != nil {
		return err
	}
	if response, getErr := client.Do(getRequest); getErr == nil {
		func() {
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode == http.StatusOK {
				var current struct {
					Version int64 `json:"version"`
				}
				if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&current) == nil {
					version = current.Version + 1
				}
			}
		}()
	}
	payloadItems := make([]map[string]any, 0, len(items))
	for _, item := range items {
		payloadItems = append(payloadItems, map[string]any{"external_item_id": item.ExternalItemID, "name": item.Name, "description": item.Description, "unit_price_minor": item.UnitPriceMinor, "available": item.Available})
	}
	payload, err := json.Marshal(map[string]any{"version": version, "currency": "RUB", "items": payloadItems})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(kitchenURL, "/")+"/partner/v1/menu", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+partnerKey)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("kitchen returned HTTP %d: %s", response.StatusCode, string(body))
	}
	return nil
}
