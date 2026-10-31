package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/pkg/snowstate/store"
)

func openSnowflake(context.Context, *config.Config, store.Observer, *slog.Logger) (store.Store, error) {
	return nil, errors.New("snowflake store is not built yet")
}
