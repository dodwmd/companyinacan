package dumps

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

// QA: a hook pod the node evicted, or one nothing can be scheduled on, says so in the error
// instead of only "failed" or "did not finish".
func TestHookErrorsNameEvictionAndUnschedulable(t *testing.T) {
	for _, c := range []struct {
		name, jobStatus string
		timeout         time.Duration
		pod             map[string]any
		want            string
	}{
		{"evicted", "failed", 10 * time.Second,
			map[string]any{"phase": "Failed", "reason": "Evicted", "message": `Usage of EmptyDir volume "work" exceeds the limit "9Gi".`},
			`(Evicted): Usage of EmptyDir volume "work" exceeds the limit`},
		{"disrupted", "failed", 10 * time.Second,
			map[string]any{"conditions": []any{map[string]any{"type": "DisruptionTarget", "status": "True", "reason": "DeletionByTaintManager", "message": "disk pressure"}}},
			"removed the pod (DeletionByTaintManager): disk pressure"},
		{"unschedulable", "", 300 * time.Millisecond,
			map[string]any{"phase": "Pending", "conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
				"message": "0/1 nodes are available: 1 node(s) had untolerated taint {node.kubernetes.io/disk-pressure: }."}}},
			"could not be scheduled (Unschedulable): 0/1 nodes are available"},
	} {
		h, api, _ := hookFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		done := make(chan struct{})
		go func() { // the Job controller: one pod in the given state
			defer close(done)
			for ctx.Err() == nil {
				jobs, _ := api.List(context.Background(), "Job", "mod-wiki", "")
				if len(jobs) == 0 {
					time.Sleep(time.Millisecond)
					continue
				}
				name := jobs[0]["metadata"].(map[string]any)["name"].(string)
				_ = api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": name + "-x", "namespace": "mod-wiki", "labels": map[string]any{"job-name": name}},
					"status":   c.pod})
				if c.jobStatus != "" {
					_ = api.MergePatch(context.Background(), "Job", "mod-wiki", name, kube.Object{"status": map[string]any{c.jobStatus: 1}})
				}
				return
			}
		}()
		err := h.RunBackup(ctx, "wiki")
		<-done
		cancel()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}
