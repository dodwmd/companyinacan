package cli

// The applied company.yaml has two writers: this CLI and the admin UI. Neither silently overwrites
// the other (specs/067-site-editor): the UI refuses a change prepared from content that moved, and
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
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/launch"
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

// kubectlFunc runs kubectl against one cluster.
type kubectlFunc func(args ...string) (string, error)

// kubectlFor runs kubectl with prefix in front of the arguments.
func kubectlFor(cmd *cobra.Command, prefix ...string) kubectlFunc {
	return func(args ...string) (string, error) {
		return newExecutor(cmd).Run(context.Background(), cluster.Cmd{Name: "kubectl", Args: append(append([]string{}, prefix...), args...)})
	}
}

func contextArgs(kubeContext string) []string {
	if kubeContext == "" {
		return nil
	}
	return []string{"--context", kubeContext}
}

// pullCluster is the cluster `can pull` reads: the one named by --context, else the server `can up`
// made for this directory (its kubeconfig, through the SSH tunnel, as `can up` and `can status`
// reach it), else kubectl's current context. done closes the tunnel.
func pullCluster(cmd *cobra.Command, dir, kubeContext string) (kubectl kubectlFunc, done func(), err error) {
	none := func() {}
	kubeconfig := provider.KubeconfigPath(dir)
	if kubeContext != "" {
		return kubectlFor(cmd, contextArgs(kubeContext)...), none, nil
	}
	if _, err := os.Stat(kubeconfig); errors.Is(err, fs.ErrNotExist) {
		return kubectlFor(cmd), none, nil
	} else if err != nil {
		return nil, nil, err
	}
	file := filepath.Join(dir, "company.yaml")
	doc, _ := config.Load(file)
	if doc == nil {
		return nil, nil, fmt.Errorf("%s cannot be read: it names the server to reach", file)
	}
	co, err := config.DecodeCompany(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", file, err)
	}
	deps := lifecycle.deps()
	deps.Redactor = provider.NewRedactor()
	tgt, err := provider.TargetFor(provider.Spec{Dir: dir, Name: co.Name, Domain: co.Domain, Options: co.Provider.Options}, provider.NewStore(dir), deps)
	if err != nil {
		return nil, nil, err
	}
	closeTunnel, err := lifecycle.access(cmd.Context(), tgt)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach the server of this directory through the SSH tunnel: %w (--context pulls from another cluster)", err)
	}
	return kubectlFor(cmd, "--kubeconfig", kubeconfig, "--context", kubeName(co.Name)), closeTunnel, nil
}

func readClusterCompany(kubectl kubectlFunc) (clusterCompany, error) {
	out, err := kubectl("-n", "can-system", "get", "configmap", "company", "--ignore-not-found", "-o", "json")
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

// syncState is what this directory knows of a cluster's document: synced is the one it last put
// into or pulled from that cluster, pushed the one `can up` last pushed. otherCluster means it has
// synced with clusters, and this is none of them.
type syncState struct {
	synced, pushed string
	otherCluster   bool
}

func syncBases(dir, uid string) (syncState, error) {
	st, err := provider.NewStore(dir).Load()
	if err != nil || st.Launch == nil {
		return syncState{}, err
	}
	synced := st.Launch.CompanyBases[uid]
	return syncState{synced: synced, pushed: st.Launch.CompanySHA256, otherCluster: synced == "" && len(st.Launch.CompanyBases) > 0}, nil
}

// unedited reports whether local holds nothing that was never applied: it is a document this
// directory put into the cluster or pulled, with or without the default bundle `can up` adds.
func (s syncState) unedited(local []byte) bool {
	known := func(b []byte) bool { d := digest(b); return d == s.synced || d == s.pushed }
	if known(local) {
		return true
	}
	bundled, added, err := launch.WithDefaultBundle(local)
	return err == nil && added && known(bundled)
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
func overwriteRefusal(dir, kubeContext string, cc clusterCompany, local []byte) error {
	if !cc.Found || cc.Data == string(local) {
		return nil
	}
	known, err := syncBases(dir, cc.UID)
	if err != nil {
		return err
	}
	if cc.dataSHA == known.synced || cc.dataSHA == known.pushed {
		return nil
	}
	// A directory that never saw this cluster, and a document the CLI wrote: as before, apply it.
	if !cc.editedInCluster() && known.synced == "" {
		return nil
	}
	who := "was changed since this directory last applied or pulled it"
	if cc.editedInCluster() {
		who = "was changed in the admin UI"
	}
	pull := "can pull"
	if kubeContext != "" {
		pull += " --context " + kubeContext
	}
	return fmt.Errorf("the company.yaml in the cluster %s, and applying this one would discard that change.\n"+
		"  to keep it: `%s` (add --out other.yaml to compare first, if you have edits of your own), then apply\n"+
		"  to discard it: `can apply --force-company`", who, pull)
}

// replaceFile writes b over path, keeping what path is: a symlink stays one (its target is
// written) and the file keeps its mode. A new file is world-readable: company.yaml holds no secrets.
func replaceFile(path string, b []byte) error {
	target, mode := path, fs.FileMode(0o644)
	switch resolved, err := filepath.EvalSymlinks(path); {
	case err == nil:
		target = resolved
		st, err := os.Stat(target)
		if err != nil {
			return err
		}
		mode = st.Mode().Perm()
	case !errors.Is(err, fs.ErrNotExist):
		return err
	default:
		if _, lerr := os.Lstat(path); lerr == nil {
			return fmt.Errorf("%s is a symlink to a file that does not exist", path)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".company-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.Write(b)
	if err := errors.Join(werr, tmp.Chmod(mode), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// keepCopy saves b beside path as path.bak, or path.bak.N: an earlier copy is never overwritten.
func keepCopy(path string, b []byte) (string, error) {
	mode := fs.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	for i := 0; i < 100; i++ {
		name := path + ".bak"
		if i > 0 {
			name += "." + strconv.Itoa(i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // the operator's company directory
		if errors.Is(err, fs.ErrExist) {
			if have, rerr := os.ReadFile(name); rerr == nil && string(have) == string(b) { //nolint:gosec // as above
				return name, nil
			}
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(b)
		return name, errors.Join(werr, f.Close())
	}
	return "", fmt.Errorf("too many copies beside %s: remove the old .bak files", path)
}

func newPullCmd(_ *globalOpts) *cobra.Command {
	var kubeContext, out string
	var force bool
	cmd := &cobra.Command{
		Use:   "pull [dir]",
		Short: "Copy the company.yaml in the cluster back into the company directory",
		Long: "Writes the applied company.yaml (ConfigMap can-system/company), with the changes made in the admin UI,\n" +
			"to <dir>/company.yaml. It reads the server `can up` made for this directory, or the cluster --context\n" +
			"names. A company.yaml with edits that were never applied, or one synced with another cluster, is not\n" +
			"overwritten without --force (which keeps a copy as company.yaml.bak). With --out the document goes to\n" +
			"that file instead (\"-\" for standard output) and nothing else changes.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			kubectl, done, err := pullCluster(cmd, dir, kubeContext)
			if err != nil {
				return failure("%v", err)
			}
			defer done()
			cc, err := readClusterCompany(kubectl)
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
				known, err := syncBases(dir, cc.UID)
				if err != nil {
					return failure("%v", err)
				}
				why := ""
				switch {
				case local == nil:
				case known.otherCluster:
					why = "was last synced with another cluster than this one"
				case !known.unedited(local):
					why = "has changes that were never applied"
				}
				if why != "" {
					if !force {
						return failure("%s %s, and pulling would overwrite it.\n"+
							"  compare first: `can pull --out cluster.yaml`\n  overwrite anyway (a copy stays in company.yaml.bak): `can pull --force`", path, why)
					}
					kept, err := keepCopy(path, local)
					if err != nil {
						return failure("%v", err)
					}
					fmt.Fprintln(w, "kept the previous company.yaml as", kept)
				}
				if err := replaceFile(path, []byte(cc.Data)); err != nil {
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
	cmd.Flags().StringVar(&kubeContext, "context", os.Getenv("CAN_DEV_CONTEXT"), "kubectl context of the cluster (default $CAN_DEV_CONTEXT, else the server of `can up`, else the current context)")
	cmd.Flags().StringVar(&out, "out", "", "write to this new file (\"-\" for standard output) instead of company.yaml")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a company.yaml that has unapplied edits, keeping company.yaml.bak")
	return cmd
}
