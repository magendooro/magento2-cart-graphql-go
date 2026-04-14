package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/magendooro/magento2-cart-graphql-go/graph"
	appconfig "github.com/magendooro/magento2-cart-graphql-go/internal/config"
	"github.com/magendooro/magento2-cart-graphql-go/internal/ctxkeys"
	carterr "github.com/magendooro/magento2-cart-graphql-go/internal/errors"
	"github.com/magendooro/magento2-cart-graphql-go/internal/payment"
	commoncache "github.com/magendooro/magento2-go-common/cache"
	commonconfig "github.com/magendooro/magento2-go-common/config"
	commondb "github.com/magendooro/magento2-go-common/database"
	commonjwt "github.com/magendooro/magento2-go-common/jwt"
	"github.com/magendooro/magento2-go-common/middleware"
)

type App struct {
	cfg   *appconfig.Config
	db    *sql.DB
	cache *commoncache.Client
}

func New(cfg *appconfig.Config) (*App, error) {
	if cfg.Logging.Pretty {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}
	level, err := zerolog.ParseLevel(cfg.Logging.Level)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	db, err := commondb.NewConnection(commondb.Config{
		Host:            cfg.Database.Host,
		Port:            cfg.Database.Port,
		User:            cfg.Database.User,
		Password:        cfg.Database.Password,
		Name:            cfg.Database.Name,
		Socket:          cfg.Database.Socket,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
		ConnMaxIdleTime: cfg.Database.ConnMaxIdleTime,
	})
	if err != nil {
		return nil, fmt.Errorf("database connection failed: %w", err)
	}
	log.Info().Str("database", cfg.Database.Name).Msg("connected to database")

	redisCache := commoncache.New(commoncache.Config{
		Host:     cfg.Redis.Host,
		Port:     cfg.Redis.Port,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
		Prefix:   "cart_gql:",
	})

	return &App{cfg: cfg, db: db, cache: redisCache}, nil
}

func (a *App) Run() error {
	storeResolver := middleware.NewStoreResolver(a.db)

	var jwtManager *commonjwt.Manager
	if a.cfg.Magento.CryptKey != "" {
		jwtManager = commonjwt.NewManager(a.cfg.Magento.CryptKey, a.cfg.Magento.JWTTTLMinutes)
		log.Info().Msg("JWT authentication enabled")
	}
	tokenResolver := middleware.NewTokenResolver(a.db, jwtManager)

	cp, err := commonconfig.NewConfigProvider(a.db)
	if err != nil {
		return fmt.Errorf("config provider failed: %w", err)
	}

	stripeClient := payment.NewStripeClient(a.cfg.Stripe.SecretKey, a.cfg.Stripe.WebhookSecret)
	if stripeClient != nil {
		log.Info().Msg("Stripe payment integration enabled")
	}

	resolver, err := graph.NewResolver(a.db, cp, stripeClient)
	if err != nil {
		return fmt.Errorf("failed to create resolver: %w", err)
	}
	resolver.TokenResolver = tokenResolver
	if stripeClient != nil {
		resolver.CartService.SetStripeClient(stripeClient)
	}

	srv := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{
		Resolvers: resolver,
	}))

	if a.cfg.GraphQL.ComplexityLimit > 0 {
		srv.Use(extension.FixedComplexityLimit(a.cfg.GraphQL.ComplexityLimit))
	}

	mux := http.NewServeMux()
	mux.Handle("/graphql", srv)
	mux.Handle("/{$}", playground.Handler("Magento Cart GraphQL", "/graphql"))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := a.db.Ping(); err != nil {
			http.Error(w, "database unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("PUT /api/orders/{increment_id}/status", a.handleOrderStatus)
	mux.HandleFunc("POST /api/orders/{increment_id}/refund", a.handleStripeRefund(stripeClient))
	mux.HandleFunc("POST /stripe/webhook", a.handleStripeWebhook(stripeClient, resolver))

	var h http.Handler = mux
	// Cart data changes on every mutation (add item, update qty, set address, etc.)
	// and the cache middleware has no invalidation — skip caching entirely.
	h = middleware.CacheMiddleware(nil, middleware.CacheOptions{})(h)
	h = middleware.AuthMiddleware(tokenResolver)(h)
	h = middleware.StoreMiddleware(storeResolver)(h)
	h = ipMiddleware(h)
	h = middleware.LoggingMiddleware(h)
	h = middleware.CORSMiddleware(h)
	h = middleware.MetricsMiddleware("cart")(h)
	h = middleware.RecoveryMiddleware(h)

	outerMux := http.NewServeMux()
	outerMux.Handle("/metrics", promhttp.Handler())
	outerMux.Handle("/", h)

	server := &http.Server{
		Addr:         ":" + a.cfg.Server.Port,
		Handler:      outerMux,
		ReadTimeout:  a.cfg.Server.ReadTimeout,
		WriteTimeout: a.cfg.Server.WriteTimeout,
		IdleTimeout:  120 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	go a.runCartCleanup(cleanupCtx, cp)

	go func() {
		log.Info().Str("port", a.cfg.Server.Port).Msg("server starting")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server failed")
		}
	}()

	<-done
	cleanupCancel()
	log.Info().Msg("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown failed: %w", err)
	}

	a.db.Close()
	if a.cache != nil {
		a.cache.Close()
	}
	log.Info().Msg("server stopped")
	return nil
}

// ipMiddleware extracts the client IP from the request and stores it in context.
func ipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := extractClientIP(r)
		ctx := ctxkeys.WithRemoteIP(r.Context(), ip)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractClientIP returns the best-effort client IP from the request.
// Respects X-Forwarded-For and X-Real-IP headers for reverse-proxy setups.
func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ── Task 95: Cart expiry cleanup ──────────────────────────────────────────────

// runCartCleanup runs a periodic job that deactivates expired guest carts.
// Interval: every 6 hours. TTL: checkout/cart/delete_quote_after (default 30 days).
func (a *App) runCartCleanup(ctx context.Context, cp *commonconfig.ConfigProvider) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	a.cleanupExpiredCarts(ctx, cp)
	for {
		select {
		case <-ticker.C:
			a.cleanupExpiredCarts(ctx, cp)
		case <-ctx.Done():
			return
		}
	}
}

func (a *App) cleanupExpiredCarts(ctx context.Context, cp *commonconfig.ConfigProvider) {
	days := cp.GetInt("checkout/cart/delete_quote_after", 1, 30)
	res, err := a.db.ExecContext(ctx,
		"UPDATE quote SET is_active = 0, updated_at = NOW() WHERE is_active = 1 AND customer_is_guest = 1 AND updated_at < DATE_SUB(NOW(), INTERVAL ? DAY)",
		days,
	)
	if err != nil {
		log.Error().Err(err).Msg("cart expiry cleanup failed")
		return
	}
	affected, _ := res.RowsAffected()
	if affected > 0 {
		log.Info().Int64("deactivated", affected).Int("days", days).Msg("expired guest carts deactivated")
	}
}

// ── Task 98: Order status update REST endpoint ────────────────────────────────

// validTransitions defines allowed status changes. Any status can be canceled.
var validTransitions = map[string]map[string]bool{
	"pending":    {"processing": true, "canceled": true, "holded": true},
	"processing": {"complete": true, "canceled": true, "holded": true},
	"holded":     {"processing": true, "canceled": true},
}

// statusToState maps an order status to its Magento state.
var statusToState = map[string]string{
	"pending":    "new",
	"processing": "processing",
	"complete":   "complete",
	"canceled":   "canceled",
	"holded":     "holded",
}

func (a *App) handleOrderStatus(w http.ResponseWriter, r *http.Request) {
	incrementID := r.PathValue("increment_id")

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Status == "" {
		http.Error(w, `{"error":"status field required"}`, http.StatusBadRequest)
		return
	}
	newStatus := body.Status

	// Load current order
	var orderID int
	var currentStatus string
	err := a.db.QueryRowContext(r.Context(),
		"SELECT entity_id, status FROM sales_order WHERE increment_id = ?", incrementID,
	).Scan(&orderID, &currentStatus)
	if err == sql.ErrNoRows {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": carterr.ErrOrderNotFound(incrementID).Error()})
		return
	}
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	// Validate transition
	if allowed, ok := validTransitions[currentStatus]; !ok || !allowed[newStatus] {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]string{"error": carterr.ErrOrderStatusInvalid(currentStatus, newStatus).Error()})
		return
	}

	newState, ok := statusToState[newStatus]
	if !ok {
		newState = newStatus
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, `{"error":"transaction error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(r.Context(),
		"UPDATE sales_order SET state = ?, status = ?, updated_at = NOW() WHERE entity_id = ?",
		newState, newStatus, orderID,
	); err != nil {
		http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
		return
	}

	if _, err := tx.ExecContext(r.Context(),
		"UPDATE sales_order_grid SET status = ?, updated_at = NOW() WHERE entity_id = ?",
		newStatus, orderID,
	); err != nil {
		http.Error(w, `{"error":"grid update failed"}`, http.StatusInternalServerError)
		return
	}

	comment := fmt.Sprintf("Order status changed from %s to %s.", currentStatus, newStatus)
	if _, err := tx.ExecContext(r.Context(),
		"INSERT INTO sales_order_status_history (parent_id, status, comment, created_at, is_customer_notified, is_visible_on_front, entity_name) VALUES (?, ?, ?, NOW(), 0, 0, 'order')",
		orderID, newStatus, comment,
	); err != nil {
		http.Error(w, `{"error":"history insert failed"}`, http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"commit failed"}`, http.StatusInternalServerError)
		return
	}

	log.Info().Str("increment_id", incrementID).Str("from", currentStatus).Str("to", newStatus).Msg("order status updated")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"increment_id": incrementID,
		"status":       newStatus,
		"state":        newState,
	})
}

// ── Task 60: Stripe webhook ───────────────────────────────────────────────────

// handleStripeWebhook handles POST /stripe/webhook.
// On checkout.session.completed: resolves cart_id from metadata → places order → stores payment_intent_id.
func (a *App) handleStripeWebhook(sc *payment.StripeClient, resolver *graph.Resolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sc == nil {
			http.Error(w, `{"error":"Stripe not configured"}`, http.StatusServiceUnavailable)
			return
		}

		event, err := sc.ParseWebhookEvent(r)
		if err != nil {
			log.Warn().Err(err).Msg("stripe webhook: rejected")
			http.Error(w, `{"error":"invalid webhook"}`, http.StatusBadRequest)
			return
		}

		if event.Type != "checkout.session.completed" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Extract session data
		var sess struct {
			ID            string            `json:"id"`
			Metadata      map[string]string `json:"metadata"`
			PaymentIntent string            `json:"payment_intent"`
			AmountTotal   int64             `json:"amount_total"`
		}
		if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
			log.Error().Err(err).Msg("stripe webhook: unmarshal session")
			http.Error(w, `{"error":"bad payload"}`, http.StatusBadRequest)
			return
		}

		maskedCartID := sess.Metadata["cart_id"]
		if maskedCartID == "" {
			log.Error().Msg("stripe webhook: missing cart_id in metadata")
			w.WriteHeader(http.StatusOK) // ack to Stripe anyway
			return
		}

		// Idempotency: check if order already placed for this session
		var existingOrderID int
		a.db.QueryRowContext(r.Context(),
			"SELECT entity_id FROM sales_order WHERE ext_order_id = ?", sess.ID,
		).Scan(&existingOrderID)
		if existingOrderID > 0 {
			log.Info().Str("session_id", sess.ID).Msg("stripe webhook: order already placed, skipping")
			w.WriteHeader(http.StatusOK)
			return
		}

		// Place the order
		output, err := resolver.CartService.PlaceOrder(r.Context(), maskedCartID)
		if err != nil {
			log.Error().Err(err).Str("cart_id", maskedCartID).Msg("stripe webhook: place order failed")
			http.Error(w, `{"error":"order placement failed"}`, http.StatusInternalServerError)
			return
		}
		if len(output.Errors) > 0 {
			log.Error().Str("error", output.Errors[0].Message).Str("cart_id", maskedCartID).Msg("stripe webhook: place order error")
			http.Error(w, `{"error":"order placement failed"}`, http.StatusInternalServerError)
			return
		}

		incrementID := ""
		if output.OrderV2 != nil {
			incrementID = output.OrderV2.Number
		}

		// Store payment_intent_id and mark session in sales_order
		if incrementID != "" && sess.PaymentIntent != "" {
			a.db.ExecContext(r.Context(), `
				UPDATE sales_order o
				JOIN sales_order_payment p ON p.parent_id = o.entity_id
				SET p.last_trans_id = ?,
				    o.ext_order_id = ?,
				    o.updated_at = NOW()
				WHERE o.increment_id = ?`,
				sess.PaymentIntent, sess.ID, incrementID,
			)
		}

		log.Info().
			Str("session_id", sess.ID).
			Str("increment_id", incrementID).
			Str("payment_intent", sess.PaymentIntent).
			Msg("stripe webhook: order placed")

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"order": incrementID})
	}
}

// ── Task 62: Stripe refund ────────────────────────────────────────────────────

// handleStripeRefund handles POST /api/orders/{increment_id}/refund.
// Body: {"amount": 10.00} (optional; omit for full refund).
func (a *App) handleStripeRefund(sc *payment.StripeClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sc == nil {
			http.Error(w, `{"error":"Stripe not configured"}`, http.StatusServiceUnavailable)
			return
		}

		incrementID := r.PathValue("increment_id")

		var body struct {
			Amount float64 `json:"amount"`
		}
		json.NewDecoder(r.Body).Decode(&body)

		// Load payment_intent_id from sales_order_payment
		var paymentIntentID string
		err := a.db.QueryRowContext(r.Context(), `
			SELECT COALESCE(p.last_trans_id, '')
			FROM sales_order o
			JOIN sales_order_payment p ON p.parent_id = o.entity_id
			WHERE o.increment_id = ?`, incrementID,
		).Scan(&paymentIntentID)
		if err == sql.ErrNoRows {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": carterr.ErrOrderNotFound(incrementID).Error()})
			return
		}
		if err != nil || paymentIntentID == "" {
			http.Error(w, `{"error":"no Stripe payment found for this order"}`, http.StatusUnprocessableEntity)
			return
		}

		amountCents := int64(body.Amount * 100)
		refundID, err := sc.CreateRefund(r.Context(), paymentIntentID, amountCents)
		if err != nil {
			log.Error().Err(err).Str("order", incrementID).Msg("stripe refund failed")
			http.Error(w, `{"error":"refund failed"}`, http.StatusInternalServerError)
			return
		}

		// Update order status to closed
		a.db.ExecContext(r.Context(),
			"UPDATE sales_order SET state = 'closed', status = 'closed', updated_at = NOW() WHERE increment_id = ?",
			incrementID,
		)

		log.Info().Str("order", incrementID).Str("refund_id", refundID).Float64("amount", body.Amount).Msg("stripe refund issued")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"refund_id":    refundID,
			"increment_id": incrementID,
			"status":       "refunded",
		})
	}
}
