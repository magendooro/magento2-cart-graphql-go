package app

import (
	"context"
	"database/sql"
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
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/magendooro/magento2-cart-graphql-go/graph"
	appconfig "github.com/magendooro/magento2-cart-graphql-go/internal/config"
	"github.com/magendooro/magento2-cart-graphql-go/internal/ctxkeys"
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
		Prefix:   "cust_gql:",
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

	resolver, err := graph.NewResolver(a.db, cp)
	if err != nil {
		return fmt.Errorf("failed to create resolver: %w", err)
	}
	resolver.TokenResolver = tokenResolver

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

	var h http.Handler = mux
	h = middleware.CacheMiddleware(a.cache, middleware.CacheOptions{
		SkipAuthenticated: true,
		SkipMutations:     true,
	})(h)
	h = middleware.AuthMiddleware(tokenResolver)(h)
	h = middleware.StoreMiddleware(storeResolver)(h)
	h = ipMiddleware(h)
	h = middleware.LoggingMiddleware(h)
	h = middleware.CORSMiddleware(h)
	h = middleware.RecoveryMiddleware(h)

	server := &http.Server{
		Addr:         ":" + a.cfg.Server.Port,
		Handler:      h,
		ReadTimeout:  a.cfg.Server.ReadTimeout,
		WriteTimeout: a.cfg.Server.WriteTimeout,
		IdleTimeout:  120 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Info().Str("port", a.cfg.Server.Port).Msg("server starting")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server failed")
		}
	}()

	<-done
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
