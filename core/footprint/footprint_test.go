package footprint

import (
	"fmt"
	"strings"
	"testing"
)

const podsJSON = `{"items":[
 {"metadata":{"namespace":"can-system","name":"site-abc","uid":"11111111-2222-3333-4444-555555555555"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"namespace":"kube-system","name":"coredns-1","uid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]}}
]}`

func sample(site, dns, node int64) string {
	return strings.Join([]string{
		fmt.Sprintf("NODE %d %d", node*MiB, 10*MiB),
		fmt.Sprintf("CG /kubepods/burstable/pod11111111-2222-3333-4444-555555555555 %d %d", site*MiB, 2*MiB),
		fmt.Sprintf("CG /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaaaaaaa_bbbb_cccc_dddd_eeeeeeeeeeee.slice %d 0", dns*MiB),
		fmt.Sprintf("CG /kubepods/besteffort/pod99999999-2222-3333-4444-555555555555 %d 0", 1*MiB),
		"PROC k3s-server 204800",
		"",
	}, "\n")
}

func TestParsePods(t *testing.T) {
	p, err := ParsePods([]byte(podsJSON))
	if err != nil || len(p) != 2 {
		t.Fatalf("%v %v", p, err)
	}
	if !p[0].Ready || p[1].Ready || p[0].Namespace != "can-system" {
		t.Fatalf("%+v", p)
	}
	if _, err := ParsePods([]byte("nope")); err == nil {
		t.Fatal("want error")
	}
}

func TestPodUID(t *testing.T) {
	for path, want := range map[string]string{
		"/kubepods/burstable/pod11111111-2222-3333-4444-555555555555":         "11111111-2222-3333-4444-555555555555",
		"/x/kubepods-burstable-podaaaaaaaa_bbbb_cccc_dddd_eeeeeeeeeeee.slice": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	} {
		if got, ok := PodUID(path); !ok || got != want {
			t.Errorf("%s: %q %v", path, got, ok)
		}
	}
	if _, ok := PodUID("/kubepods/burstable"); ok {
		t.Error("qos cgroup is not a pod")
	}
}

func TestParseRawErrors(t *testing.T) {
	for _, bad := range []string{"", "CG /x 1 2\n", "NODE 1\n", "NODE a b\n", "WAT 1 2\nNODE 1 1\n", "NODE 1 1\nCG /x -1 0\n"} {
		if _, err := ParseRaw(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestBuild(t *testing.T) {
	pods, _ := ParsePods([]byte(podsJSON))
	var raws []Raw
	for _, s := range [][3]int64{{50, 20, 400}, {60, 20, 410}, {52, 20, 405}} {
		r, err := ParseRaw(sample(s[0], s[1], s[2]))
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, r)
	}
	r, err := Build("t", pods, raws)
	if err != nil {
		t.Fatal(err)
	}
	if r.Samples != 3 || len(r.Pods) != 2 || r.Unmapped != 3 {
		t.Fatalf("%+v", r)
	}
	// working set = current - inactive_file (2): site series 48, 58, 50 -> median 50, max 58
	if r.Pods[0].Name != "site-abc" || r.Pods[0].MedianMB != 50 || r.Pods[0].MaxMB != 58 {
		t.Fatalf("%+v", r.Pods)
	}
	if r.K3sRSS == nil || r.K3sRSS.MedianMB != 200 {
		t.Fatalf("%+v", r.K3sRSS)
	}
	if r.Node.MedianMB != 395 { // 405 - 10
		t.Fatalf("node %+v", r.Node)
	}
	if len(r.ByNS) != 2 || r.ByNS[0].Namespace != "can-system" {
		t.Fatalf("%+v", r.ByNS)
	}
	if _, err := Build("t", pods, nil); err == nil {
		t.Fatal("no samples must fail")
	}
}

func TestCheck(t *testing.T) {
	r := Report{Pods: []PodStat{{Namespace: "mod-forms", Name: "forms-1", MedianMB: 17}, {Namespace: "mod-forms", Name: "forms-job", MedianMB: 1}, {Namespace: "other", Name: "x", MedianMB: 900}}}
	res, err := Check(r, Selector{Namespace: "mod-forms"}, 15, DefaultTolerance)
	if err != nil || !res.Pass || res.ActualMB != 18 || res.LimitMB != 18 { // the exact 20% boundary passes
		t.Fatalf("%+v %v", res, err)
	}
	r.Pods[0].MedianMB = 17.5
	res, _ = Check(r, Selector{Namespace: "mod-forms"}, 15, DefaultTolerance)
	if res.Pass || !strings.Contains(res.Message, "OVER") {
		t.Fatalf("%+v", res)
	}
	res, err = Check(r, Selector{Namespace: "mod-forms", NamePrefix: "forms-1"}, 15, DefaultTolerance)
	if err != nil || !res.Pass || res.Pods != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, c := range []struct {
		sel Selector
		mb  int
		tol float64
	}{{Selector{Namespace: "nope"}, 15, .2}, {Selector{}, 15, .2}, {Selector{Namespace: "other"}, 0, .2}, {Selector{Namespace: "other"}, 5, -1}} {
		if _, err := Check(r, c.sel, c.mb, c.tol); err == nil {
			t.Errorf("%+v: want error", c)
		}
	}
}

func TestParseTop(t *testing.T) {
	rows, err := ParseTop("can-system site-1 3m 12Mi\nkube-system coredns-1 2m 20480Ki\n\n")
	if err != nil || len(rows) != 2 || rows[0].MB != 12 || rows[1].MB != 20 {
		t.Fatalf("%+v %v", rows, err)
	}
	for _, bad := range []string{"a b c\n", "a b 1m 12M\n", "a b 1m xMi\n"} {
		if _, err := ParseTop(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	r := FromTop("x", rows)
	if r.Source != "kubectl-top" || len(r.ByNS) != 2 {
		t.Fatalf("%+v", r)
	}
}
