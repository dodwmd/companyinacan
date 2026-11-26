package runtime

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/adminui"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

// formsModule is the in-house provider of `leads`; its API token is the only one can-core hands out
// by itself.
const formsModule = "forms"

// leadsToken is the credential of the admin UI's leads view. CAN_LEADS_TOKEN_FILE wins; without it
// a can-core that installs modules reads the token it generated for the forms module from that
// module's Secret, so a fresh `can up` needs no further step.
func (c Config) leadsToken(reg adminui.Resolver, log *slog.Logger) func() (string, error) {
	if c.LeadsTokenFile != "" || c.Installer != "kubernetes" || c.ModuleInstaller != nil || c.DevStack {
		return adminui.FileToken(c.LeadsTokenFile)
	}
	api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
	if err != nil {
		log.Warn("leads: the forms token cannot be read from the cluster", "error", err)
		return adminui.FileToken("")
	}
	return formsToken(reg, api)
}

// formsToken reads the token on every call, so a rotation needs no restart. It is handed out only
// while the forms module itself is the one provider of `leads`: another provider never sees it.
func formsToken(reg adminui.Resolver, api kube.API) func() (string, error) {
	return func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := reg.Resolve(ctx, "leads")
		if err != nil || len(res.Providers) != 1 || res.Providers[0].ModuleID != formsModule {
			return "", adminui.ErrLeadsUnavailable
		}
		o, err := api.Get(ctx, "Secret", "mod-"+formsModule, "can-module")
		if err != nil {
			return "", adminui.ErrLeadsUnavailable
		}
		data, _ := o["data"].(map[string]any)
		enc, _ := data["CAN_FORMS_API_TOKEN"].(string)
		b, err := base64.StdEncoding.DecodeString(enc)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return "", adminui.ErrLeadsUnavailable
		}
		return strings.TrimSpace(string(b)), nil
	}
}
