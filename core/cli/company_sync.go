package cli

// The applied company.yaml has two writers: this CLI and the admin UI. Neither silently overwrites
// the other (specs/064-site-editor): the UI refuses a change prepared from content that moved, and
// here `can apply` refuses to replace a document it has not seen. `can pull` brings it back.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
)

// clusterCompany is ConfigMap can-system/company.
type clusterCompany struct {
	Found                     bool
	Data                      string
	UID, ResourceVersion      string
	serverEditSHA256, dataSHA string
}

// editedInCluster reports whether can-core wrote the document that is there now.
func (c clusterCompany) editedInCluster() bool {
	return c.serverEditSHA256 != "" && c.serverEditSHA256 == c.dataSHA
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func readClusterCompany(cmd *cobra.Command, kubeContext string) (clusterCompany, error) {
	args := []string{"-n", "can-system", "get", "configmap", "company", "--ignore-not-found", "-o", "json"}
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}
	out, err := newExecutor(cmd).Run(context.Background(), cluster.Cmd{Name: "kubectl", Args: args})
	if err != nil {
		return clusterCompany{}, fmt.Errorf("cannot read the company.yaml in the cluster, so cannot tell whether applying would overwrite changes: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		return clusterCompany{}, nil // there is none yet
	}
	var cm struct {
		Metadata struct {
			UID             string            `json:"uid"`
			ResourceVersion string            `json:"resourceVersion"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &cm); err != nil {
		return clusterCompany{}, fmt.Errorf("ConfigMap can-system/company is not readable: %w", err)
	}
	data := cm.Data["company.yaml"]
	return clusterCompany{
		Found: data != "", Data: data, UID: cm.Metadata.UID, ResourceVersion: cm.Metadata.ResourceVersion,
		serverEditSHA256: cm.Metadata.Annotations[modmgr.ServerEditAnnotation], dataSHA: digest([]byte(data)),
	}, nil
}

// syncBases are what this directory knows of the cluster's document: synced is the one it last
// applied to or pulled from that cluster, pushed the one `can up` last pushed.
func syncBases(dir, uid string) (synced, pushed string, err error) {
	st, err := provider.NewStore(dir).Load()
	if err != nil || st.Launch == nil {
		return "", "", err
	}
	return st.Launch.CompanyBases[uid], st.Launch.CompanySHA256, nil
}

func recordBase(dir, uid string, doc []byte) error {
	if uid == "" {
		return nil
	}
	return provider.NewStore(dir).Mutate(func(s *provider.State) error {
		if s.Launch == nil {
			s.Launch = &provider.LaunchState{}
		}
		if s.Launch.CompanyBases == nil || len(s.Launch.CompanyBases) > 20 {
			s.Launch.CompanyBases = map[string]string{}
		}
		s.Launch.CompanyBases[uid] = digest(doc)
		return nil
	})
}

// overwriteRefusal says why replacing cc with local would discard changes this directory has not
// seen, nil when it would not.
func overwriteRefusal(dir string, cc clusterCompany, local []byte) error {
	if !cc.Found || cc.Data == string(local) {
		return nil
	}
	synced, pushed, err := syncBases(dir, cc.UID)
	if err != nil {
		return err
	}
	if cc.dataSHA == synced || cc.dataSHA == pushed {
		return nil
	}
	// A directory that never saw this cluster, and a document the CLI wrote: as before, apply it.
	if !cc.editedInCluster() && synced == "" {
		return nil
	}
	who := "was changed since this directory last applied or pulled it"
	if cc.editedInCluster() {
		who = "was changed in the admin UI"
	}
	return fmt.Errorf("the company.yaml in the cluster %s, and applying this one would discard that change.\n"+
		"  to keep it: `can pull` (add --out other.yaml to compare first, if you have edits of your own), then apply\n"+
		"  to discard it: `can apply --force-company`", who)
}

func newPullCmd(_ *globalOpts) *cobra.Command {
	var kubeContext, out string
	var force bool
	cmd := &cobra.Command{
		Use:   "pull [dir]",
		Short: "Copy the company.yaml in the cluster back into the company directory",
		Long: "Writes the applied company.yaml (ConfigMap can-system/company), with the changes made in the admin UI,\n" +
			"to <dir>/company.yaml. A company.yaml with edits that were never applied is not overwritten without\n" +
			"--force (which keeps a copy as company.yaml.bak). With --out the document goes to that file instead\n" +
			"(\"-\" for standard output) and nothing else changes.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			cc, err := readClusterCompany(cmd, kubeContext)
			if err != nil {
				return failure("%v", err)
			}
			if !cc.Found {
				return failure("there is no company.yaml in the cluster (ConfigMap can-system/company): nothing to pull")
			}
			w := cmd.OutOrStdout()
			switch out {
			case "":
			case "-":
				_, err := fmt.Fprint(w, cc.Data)
				return err
			default:
				f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // the operator's file; the document holds no secrets
				if err != nil {
					return failure("%v", err)
				}
				_, werr := f.WriteString(cc.Data)
				if err := errors.Join(werr, f.Close()); err != nil {
					return failure("%v", err)
				}
				fmt.Fprintln(w, "wrote the cluster's company.yaml to", out)
				return nil
			}
			path := filepath.Join(dir, "company.yaml")
			local, err := os.ReadFile(path) //nolint:gosec // the operator's company directory
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return failure("%v", err)
			}
			switch {
			case string(local) == cc.Data:
				fmt.Fprintln(w, "company.yaml is up to date")
			default:
				synced, pushed, err := syncBases(dir, cc.UID)
				if err != nil {
					return failure("%v", err)
				}
				if sha := digest(local); local != nil && sha != synced && sha != pushed {
					if !force {
						return failure("%s has changes that were never applied, which pulling would overwrite.\n"+
							"  compare first: `can pull --out cluster.yaml`\n  overwrite anyway (a copy stays in company.yaml.bak): `can pull --force`", path)
					}
					if err := writeFileAtomic(path+".bak", local); err != nil {
						return failure("%v", err)
					}
				}
				if err := writeFileAtomic(path, []byte(cc.Data)); err != nil {
					return failure("%v", err)
				}
				fmt.Fprintln(w, "updated", path, "from the cluster; review it and commit it")
			}
			if err := recordBase(dir, cc.UID, []byte(cc.Data)); err != nil {
				return failure("%v", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeContext, "context", os.Getenv("CAN_DEV_CONTEXT"), "kubectl context of the cluster (default $CAN_DEV_CONTEXT, else the current context)")
	cmd.Flags().StringVar(&out, "out", "", "write to this new file (\"-\" for standard output) instead of company.yaml")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a company.yaml that has unapplied edits, keeping company.yaml.bak")
	return cmd
}
