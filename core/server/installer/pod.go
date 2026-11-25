package installer

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// refs are the names a pod template may refer to: objects the chart itself defines, and the Secrets
// can-core creates for the module (plus the Secret names the chart declares under .Values.secrets,
// which can-core is expected to provide). Nothing else in the namespace (the release record, or any
// Secret someone put there) can be mounted or read as environment.
type refs struct {
	secrets, configMaps, pvcs map[string]bool
}

func newRefs(objs []kube.Object, values map[string]any, s reconcile.Spec) refs {
	r := refs{secrets: map[string]bool{secretModule: true}, configMaps: map[string]bool{}, pvcs: map[string]bool{}}
	if k := s.Manifest.Requires.Database; k == "postgres" || k == "mariadb" {
		r.secrets[secretDB] = true
	}
	for _, o := range objs {
		switch o["kind"] {
		case "Secret":
			r.secrets[nameOf(o)] = true
		case "ConfigMap":
			r.configMaps[nameOf(o)] = true
		case "PersistentVolumeClaim":
			r.pvcs[nameOf(o)] = true
		}
	}
	for _, v := range asMap(values["secrets"]) {
		if n, ok := v.(string); ok && n != "" && n != secretRelease {
			r.secrets[n] = true
		}
	}
	delete(r.secrets, secretRelease)
	return r
}

// boolish reads a YAML boolean the way a hostile chart might write it ("true", "True", 1, ...).
func boolish(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes", "on", "y", "t":
			return true
		}
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return false
}

func checkRef(kind string, name any, allowed map[string]bool) error {
	n, _ := name.(string)
	if n == "" || !allowed[n] {
		return fmt.Errorf("%s %q is not defined by this chart or provided by can-core", kind, n)
	}
	return nil
}

func (r refs) checkEnv(c map[string]any) error {
	for _, e := range asList(c["env"]) {
		vf := asMap(asMap(e)["valueFrom"])
		for k, sel := range vf {
			switch k {
			case "secretKeyRef":
				if err := checkRef("Secret", asMap(sel)["name"], r.secrets); err != nil {
					return err
				}
			case "configMapKeyRef":
				if err := checkRef("ConfigMap", asMap(sel)["name"], r.configMaps); err != nil {
					return err
				}
			case "fieldRef", "resourceFieldRef":
			default:
				return fmt.Errorf("env valueFrom.%s is not allowed", k)
			}
		}
	}
	for _, e := range asList(c["envFrom"]) {
		em := asMap(e)
		if sr := asMap(em["secretRef"]); sr != nil {
			if err := checkRef("Secret", sr["name"], r.secrets); err != nil {
				return err
			}
		}
		if cr := asMap(em["configMapRef"]); cr != nil {
			if err := checkRef("ConfigMap", cr["name"], r.configMaps); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r refs) checkVolume(v any) (keep bool, err error) {
	vm := asMap(v)
	name, _ := vm["name"].(string)
	var sources []string
	for k := range vm {
		if k != "name" {
			sources = append(sources, k)
		}
	}
	if len(sources) != 1 {
		return false, fmt.Errorf("volume %q must have exactly one source", name)
	}
	switch sources[0] {
	case "emptyDir":
	case "configMap":
		return true, checkRef("ConfigMap", asMap(vm["configMap"])["name"], r.configMaps)
	case "secret":
		return true, checkRef("Secret", asMap(vm["secret"])["secretName"], r.secrets)
	case "persistentVolumeClaim":
		return true, checkRef("PersistentVolumeClaim", asMap(vm["persistentVolumeClaim"])["claimName"], r.pvcs)
	case "projected":
		// Only the `can` token; can-core injects its own (same name), so the chart's copy is dropped.
		for _, src := range asList(asMap(vm["projected"])["sources"]) {
			t := asMap(asMap(src)["serviceAccountToken"])
			if t == nil || t["audience"] != "can" {
				return false, fmt.Errorf("projected volume %q: only serviceAccountToken with audience `can` is allowed", name)
			}
		}
		return name != tokenVolume, nil
	default:
		return false, fmt.Errorf("volume %q: type %s is not allowed (configMap, secret, emptyDir, persistentVolumeClaim, projected `can` token)", name, sources[0])
	}
	return true, nil
}

// hardenWorkload enforces the module contract on every pod template, whatever the chart says: own
// ServiceAccount, no API token automount, only allowlisted volume types and references, no host
// access, only the projected `can` token, and the can-core Secrets as environment. It also pins
// replicas/suspend so that disable-then-enable restores them.
func (in *Installer) hardenWorkload(o kube.Object, s reconcile.Spec, r refs) error {
	if o["kind"] == "StatefulSet" {
		for _, t := range asList(asMap(o["spec"])["volumeClaimTemplates"]) {
			if err := checkPVCSpec("volumeClaimTemplate", asMap(asMap(t)["spec"])); err != nil {
				return err
			}
		}
	}
	ps := podSpec(o)
	if ps == nil {
		return nil
	}
	for _, f := range []string{"hostNetwork", "hostPID", "hostIPC", "shareProcessNamespace"} {
		if boolish(ps[f]) {
			return fmt.Errorf("%s is not allowed", f)
		}
	}
	if _, ok := ps["ephemeralContainers"]; ok {
		return errors.New("ephemeralContainers are not allowed")
	}
	if sa, _ := ps["serviceAccountName"].(string); sa != "" && sa != s.ServiceAccount {
		return fmt.Errorf("serviceAccountName %q: modules run as %q", sa, s.ServiceAccount)
	}
	if sa, _ := ps["serviceAccount"].(string); sa != "" && sa != s.ServiceAccount {
		return fmt.Errorf("serviceAccount %q: modules run as %q", sa, s.ServiceAccount)
	}
	delete(ps, "serviceAccount")
	var vols []any
	for _, v := range asList(ps["volumes"]) {
		keep, err := r.checkVolume(v)
		if err != nil {
			return err
		}
		if keep {
			vols = append(vols, v)
		}
	}
	ps["serviceAccountName"] = s.ServiceAccount
	ps["automountServiceAccountToken"] = false
	vols = append(vols, map[string]any{"name": tokenVolume, "projected": map[string]any{"sources": []any{
		map[string]any{"serviceAccountToken": map[string]any{"audience": "can", "expirationSeconds": 3600, "path": "token"}}}}})
	ps["volumes"] = vols
	envFrom := []any{map[string]any{"secretRef": map[string]any{"name": secretModule}}}
	if k := s.Manifest.Requires.Database; k == "postgres" || k == "mariadb" {
		envFrom = append(envFrom, map[string]any{"secretRef": map[string]any{"name": secretDB}})
	}
	for _, group := range []string{"containers", "initContainers"} {
		for _, c := range asList(ps[group]) {
			cm := asMap(c)
			if cm == nil {
				continue
			}
			if boolish(asMap(cm["securityContext"])["privileged"]) {
				return errors.New("privileged containers are not allowed")
			}
			for _, p := range asList(cm["ports"]) {
				if hp := asMap(p)["hostPort"]; hp != nil && hp != 0 && hp != 0.0 {
					return errors.New("hostPort is not allowed")
				}
			}
			if err := r.checkEnv(cm); err != nil {
				return err
			}
			var mounts []any
			for _, m := range asList(cm["volumeMounts"]) {
				if asMap(m)["name"] != tokenVolume { // the chart's own copy of the token mount is replaced
					mounts = append(mounts, m)
				}
			}
			cm["volumeMounts"] = append(mounts, map[string]any{"name": tokenVolume, "mountPath": TokenDir, "readOnly": true})
			cm["envFrom"] = append(asList(cm["envFrom"]), envFrom...)
		}
	}
	switch o["kind"] {
	case "Deployment", "StatefulSet":
		spec := o["spec"].(map[string]any)
		if _, ok := spec["replicas"]; !ok {
			spec["replicas"] = 1
		}
	case "CronJob":
		spec := o["spec"].(map[string]any)
		if _, ok := spec["suspend"]; !ok {
			spec["suspend"] = false
		}
	}
	return nil
}

// checkPVCSpec refuses the fields that bind a claim to something it was not meant to reach: another
// volume (volumeName), a selector over existing volumes, or a data source; and non-default storage classes.
func checkPVCSpec(what string, spec map[string]any) error {
	for _, f := range []string{"volumeName", "selector", "dataSource", "dataSourceRef"} {
		if _, ok := spec[f]; ok {
			return fmt.Errorf("%s: %s is not allowed", what, f)
		}
	}
	if sc, ok := spec["storageClassName"]; ok && sc != nil && sc != "" && sc != "local-path" {
		return fmt.Errorf("%s: storageClassName %v is not allowed (default or local-path)", what, sc)
	}
	return nil
}
