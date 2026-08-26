package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/orders"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/partners"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/restaurantdemo"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/serviceerror"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func startPostgres(t *testing.T, migrations string) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in short mode")
	}
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"), tcpostgres.WithUsername("test"), tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Skipf("Docker/PostgreSQL unavailable: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	connectionString, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", connectionString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	directory, err := filepath.Abs(migrations)
	if err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(db, directory); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := goose.DownTo(db, directory, 0); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := goose.Up(db, directory); err != nil {
		t.Fatalf("migrate up after rollback: %v", err)
	}
	pool, err := pgxpool.New(ctx, connectionString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestKitchenOrderOutboxIdempotencyAndSkipLocked(t *testing.T) {
	pool := startPostgres(t, "../../migrations/kitchen")
	ctx := context.Background()
	partnerService := partners.New(pool)
	restaurantID, err := partnerService.Authenticate(ctx, "demo-partner-key-change-me")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := partnerService.PublishMenu(ctx, restaurantID, 1, "RUB", []partners.PublishItem{{ExternalItemID: "one", Name: "One", UnitPriceMinor: 10000, Available: true}})
	if err != nil {
		t.Fatal(err)
	}
	if publication.Version != 1 {
		t.Fatalf("unexpected version: %d", publication.Version)
	}
	var menuItemID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM menu_items WHERE restaurant_id = $1 AND external_item_id = 'one'`, restaurantID).Scan(&menuItemID); err != nil {
		t.Fatal(err)
	}
	orderService := orders.New(pool)
	userID := uuid.New()
	request := []orders.RequestedItem{{MenuItemID: menuItemID, Quantity: 2, SeenUnitPriceMinor: 10000}}
	created, err := orderService.Create(ctx, userID, restaurantID, "first-key", request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Order.TotalPriceMinor != 20000 || created.Replayed {
		t.Fatalf("unexpected create result: %+v", created)
	}
	var items, history, events int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM order_items WHERE order_id=$1), (SELECT count(*) FROM order_status_history WHERE order_id=$1), (SELECT count(*) FROM outbox_events WHERE aggregate_id=$1)`, created.Order.ID).Scan(&items, &history, &events); err != nil {
		t.Fatal(err)
	}
	if items != 1 || history != 1 || events != 1 {
		t.Fatalf("order transaction incomplete: items=%d history=%d events=%d", items, history, events)
	}
	replay, err := orderService.Create(ctx, userID, restaurantID, "first-key", request)
	if err != nil || !replay.Replayed || replay.Order.ID != created.Order.ID {
		t.Fatalf("idempotent replay failed: %+v %v", replay, err)
	}
	_, err = orderService.Create(ctx, userID, restaurantID, "first-key", []orders.RequestedItem{{MenuItemID: menuItemID, Quantity: 1, SeenUnitPriceMinor: 10000}})
	var serviceErr *serviceerror.Error
	if !errors.As(err, &serviceErr) || serviceErr.Code != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
	if _, err := orderService.Cancel(ctx, created.Order.ID, userID); err != nil {
		t.Fatal(err)
	}
	var eventStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM outbox_events WHERE aggregate_id=$1`, created.Order.ID).Scan(&eventStatus); err != nil {
		t.Fatal(err)
	}
	if eventStatus != "cancelled" {
		t.Fatalf("cancel did not stop outbox: %s", eventStatus)
	}
	if _, err := orderService.Cancel(ctx, created.Order.ID, userID); err == nil {
		t.Fatal("terminal order was cancelled twice")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO restaurants (external_id, name, address) VALUES ('bad-currency-test', 'Bad', 'Address')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO menus (restaurant_id, version, payload_hash, currency) SELECT id, 1, repeat(E'\\001',32)::bytea, 'USD' FROM restaurants WHERE external_id='bad-currency-test'`); err == nil {
		t.Fatal("currency CHECK accepted USD")
	}

	second, err := orderService.Create(ctx, userID, restaurantID, "second-key", request)
	if err != nil {
		t.Fatal(err)
	}
	third, err := orderService.Create(ctx, userID, restaurantID, "third-key", request)
	if err != nil {
		t.Fatal(err)
	}
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	var firstClaim uuid.UUID
	if err := tx1.QueryRow(ctx, `SELECT id FROM outbox_events WHERE status='pending' ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&firstClaim); err != nil {
		t.Fatal(err)
	}
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	var secondClaim uuid.UUID
	if err := tx2.QueryRow(ctx, `SELECT id FROM outbox_events WHERE status='pending' ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&secondClaim); err != nil {
		t.Fatal(err)
	}
	if firstClaim == secondClaim {
		t.Fatal("SKIP LOCKED returned the same event")
	}
	if second.Order.ID == third.Order.ID {
		t.Fatal("sanity check failed: distinct orders expected")
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	raceOrder, err := orderService.Create(ctx, userID, restaurantID, "cancel-claim-race", request)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var cancelErr, claimErr error
	var raceGroup sync.WaitGroup
	raceGroup.Add(2)
	go func() {
		defer raceGroup.Done()
		<-start
		_, cancelErr = orderService.Cancel(ctx, raceOrder.Order.ID, userID)
	}()
	go func() {
		defer raceGroup.Done()
		<-start
		tx, beginErr := pool.Begin(ctx)
		if beginErr != nil {
			claimErr = beginErr
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var eventID uuid.UUID
		claimErr = tx.QueryRow(ctx, `
			UPDATE outbox_events SET status='processing', attempts=attempts+1, locked_at=now()
			WHERE id = (
				SELECT id FROM outbox_events WHERE aggregate_id=$1 AND status='pending'
				FOR UPDATE SKIP LOCKED
			) RETURNING id`, raceOrder.Order.ID).Scan(&eventID)
		if errors.Is(claimErr, pgx.ErrNoRows) {
			return
		}
		if claimErr != nil {
			return
		}
		if _, claimErr = tx.Exec(ctx, `UPDATE orders SET dispatch_started_at=now() WHERE id=$1`, raceOrder.Order.ID); claimErr != nil {
			return
		}
		claimErr = tx.Commit(ctx)
	}()
	close(start)
	raceGroup.Wait()
	var finalOrderStatus, finalEventStatus string
	var dispatchStarted *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT o.status, o.dispatch_started_at, e.status
		FROM orders o JOIN outbox_events e ON e.aggregate_id=o.id WHERE o.id=$1`, raceOrder.Order.ID).
		Scan(&finalOrderStatus, &dispatchStarted, &finalEventStatus); err != nil {
		t.Fatal(err)
	}
	if cancelErr == nil {
		if !errors.Is(claimErr, pgx.ErrNoRows) || finalOrderStatus != "cancelled" || finalEventStatus != "cancelled" {
			t.Fatalf("cancel won but state is inconsistent: claim=%v order=%s event=%s", claimErr, finalOrderStatus, finalEventStatus)
		}
	} else {
		var cancellationProblem *serviceerror.Error
		if claimErr != nil || !errors.As(cancelErr, &cancellationProblem) || cancellationProblem.Code != "ORDER_NOT_CANCELLABLE" || dispatchStarted == nil || finalEventStatus != "processing" {
			t.Fatalf("claim won but state is inconsistent: cancel=%v claim=%v order=%s event=%s dispatch=%v", cancelErr, claimErr, finalOrderStatus, finalEventStatus, dispatchStarted)
		}
	}
}

func TestRestaurantIdempotencyAndConcurrentLastItem(t *testing.T) {
	pool := startPostgres(t, "../../migrations/restaurant")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE items SET available_quantity=1, reserved_quantity=0 WHERE external_item_id='pepperoni'`); err != nil {
		t.Fatal(err)
	}
	service := restaurantdemo.New(pool)
	makeRequest := func(id uuid.UUID) restaurantdemo.IncomingRequest {
		return restaurantdemo.IncomingRequest{PlatformOrderID: id, PlatformRestaurantID: uuid.MustParse(restaurantdemo.DemoRestaurantID), CreatedAt: time.Now().UTC(), TotalPriceMinor: 69000, Currency: "RUB", Items: []restaurantdemo.IncomingItem{{PlatformMenuItemID: uuid.New(), ExternalItemID: "pepperoni", Name: "Pepperoni", UnitPriceMinor: 69000, Quantity: 1, LineTotalMinor: 69000, Currency: "RUB"}}}
	}
	requests := []restaurantdemo.IncomingRequest{makeRequest(uuid.New()), makeRequest(uuid.New())}
	start := make(chan struct{})
	results := make([]restaurantdemo.Decision, 2)
	errorsFound := make([]error, 2)
	var group sync.WaitGroup
	for index := range requests {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			results[i], errorsFound[i] = service.Receive(ctx, requests[i])
		}(index)
	}
	close(start)
	group.Wait()
	accepted, rejected := 0, 0
	acceptedIndex := 0
	for index, result := range results {
		if errorsFound[index] != nil {
			t.Fatal(errorsFound[index])
		}
		switch result.Status {
		case "accepted":
			accepted++
			acceptedIndex = index
		case "rejected":
			rejected++
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("last item reserved incorrectly: accepted=%d rejected=%d", accepted, rejected)
	}
	replay, err := service.Receive(ctx, requests[acceptedIndex])
	if err != nil || !replay.Replayed || replay.Status != "accepted" {
		t.Fatalf("restaurant replay failed: %+v %v", replay, err)
	}
	var available, reserved int
	if err := pool.QueryRow(ctx, `SELECT available_quantity, reserved_quantity FROM items WHERE external_item_id='pepperoni'`).Scan(&available, &reserved); err != nil {
		t.Fatal(err)
	}
	if available != 0 || reserved != 1 {
		t.Fatalf("stock was charged more than once: available=%d reserved=%d", available, reserved)
	}
}
