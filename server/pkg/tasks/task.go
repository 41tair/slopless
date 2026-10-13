package tasks

import (
	"context"
	"log/slog"
	"time"

	"github.com/41tair/slopless/pkg/service"
)

func Run(ctx context.Context, svc *service.Service) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := svc.Cleanup(ctx); err != nil && ctx.Err() == nil {
			slog.Error("clean expired records", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
