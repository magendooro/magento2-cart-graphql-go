package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"sync"

	"github.com/rs/zerolog/log"
)

type contextKey string

const StoreIDKey contextKey = "store_id"

type StoreResolver struct {
	db    *sql.DB
	cache map[string]int
	mu    sync.RWMutex
}

func NewStoreResolver(db *sql.DB) *StoreResolver {
	return &StoreResolver{
		db:    db,
		cache: make(map[string]int),
	}
}

func (sr *StoreResolver) Resolve(code string) (int, error) {
	sr.mu.RLock()
	if id, ok := sr.cache[code]; ok {
		sr.mu.RUnlock()
		return id, nil
	}
	sr.mu.RUnlock()

	var storeID int
	err := sr.db.QueryRow("SELECT store_id FROM store WHERE code = ? AND is_active = 1", code).Scan(&storeID)
	if err != nil {
		return 0, err
	}

	sr.mu.Lock()
	sr.cache[code] = storeID
	sr.mu.Unlock()

	return storeID, nil
}

func StoreMiddleware(resolver *StoreResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			storeCode := r.Header.Get("Store")
			if storeCode == "" {
				storeCode = "default"
			}

			storeID, err := resolver.Resolve(storeCode)
			if err != nil {
				log.Warn().Str("store_code", storeCode).Err(err).Msg("Failed to resolve store, using default (0)")
				storeID = 0
			}

			ctx := context.WithValue(r.Context(), StoreIDKey, storeID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func GetStoreID(ctx context.Context) int {
	if id, ok := ctx.Value(StoreIDKey).(int); ok {
		return id
	}
	return 0
}
