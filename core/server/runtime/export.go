package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/companyexport"
	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// ExportSocket is the Unix socket `can-core export` talks to, next to the state database.
func ExportSocket(dbPath string) string { return dirOf(dbPath) + "/export.sock" }

// companyExport builds `can export` (specs/070-company-export). Module data needs export Jobs,
// so it needs the Kubernetes installer.
func (c Config) companyExport(db *store.DB, al *audit.Log, dumper *dumps.Dumper, log *slog.Logger) (*companyexport.Service, error) {
	s := &companyexport.Service{DBPath: c.DBPath, CompanyFile: c.CompanyFile, Company: c.CompanyName, Version: c.Version,
		Audit: al, Modules: db.ListModules, Log: log}
	if dumper != nil {
		s.DumpRunning = dumper.Active
	}
	if c.Installer == "kubernetes" && c.ModuleInstaller == nil {
		api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
		if err != nil {
			return nil, err
		}
		s.Hooks = c.kubeHooks(api, db)
		s.NeverInstalled = neverInstalled(api, db)
		s.Site = publishedSite(api)
	}
	return s, nil
}

// publishedSite reads the archive the site serves (deploy/site).
func publishedSite(api kube.API) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		o, err := api.Get(ctx, "ConfigMap", "can-system", "site-content")
		if kube.IsNotFound(err) {
			return nil, errors.New("no site has been published to this server")
		} else if err != nil {
			return nil, fmt.Errorf("read the published site: %w", err)
		}
		data, _ := o["binaryData"].(map[string]any)
		enc, _ := data["site.tar.gz"].(string)
		b, err := base64.StdEncoding.DecodeString(enc)
		if err != nil || len(b) == 0 {
			return nil, errors.New("no site has been published to this server")
		}
		return b, nil
	}
}

// serveExports answers `can-core export` until ctx ends. A socket that cannot be opened only
// means this can-core cannot be exported from the command line.
func serveExports(ctx context.Context, wg *sync.WaitGroup, s *companyexport.Service, dbPath string, log *slog.Logger) {
	l, err := companyexport.Listen(ExportSocket(dbPath))
	if err != nil {
		log.Warn("`can export` is not available from the command line", "error", err)
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.Serve(ctx, l); err != nil {
			log.Error("export socket failed", "error", err)
		}
	}()
}
