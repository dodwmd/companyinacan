package runtime

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/dodwmd/companyinacan/core/server/erasure"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// erasureCollector is the collector of the modules' erasure tombstones, nil where there is nothing
// to collect from or nowhere to put it (no cluster installer, or no dumps directory).
func (c Config) erasureCollector(db *store.DB, log *slog.Logger) (*erasure.Collector, error) {
	if c.DumpsDir == "" || c.Installer != "kubernetes" || c.ModuleInstaller != nil || c.DevStack {
		return nil, nil
	}
	api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
	if err != nil {
		return nil, err
	}
	return &erasure.Collector{Modules: db.ListModules, API: api, Dir: c.DumpsDir, Log: log}, nil
}

// erasureInterval is CAN_ERASURE_INTERVAL_SECONDS, default one minute.
func erasureInterval(c Config) time.Duration {
	if n, err := strconv.Atoi(c.env("CAN_ERASURE_INTERVAL_SECONDS")); err == nil && n >= 5 && n <= 3600 {
		return time.Duration(n) * time.Second
	}
	return time.Minute
}
