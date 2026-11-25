package work

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Keys built at run time. Each family must have a message for every possible value.
var dynamicMessageFamilies = map[string][]string{
	"type.":     IssueTypes,
	"sort.":     uiSorts,
	"sprint.":   {"planned", "active", "closed"},
	"linktype.": LinkTypes,
	"status.":   {"todo"},
	"activity.": {"created", "transitioned", "updated", "commented", "ranked", "linked", "unlinked", "attachment_added", "attachment_removed"},
	"flash.":    {"created", "updated", "moved", "ranked", "commented", "linked", "unlinked", "watching", "unwatched", "sprint_created", "sprint_started", "sprint_completed", "logged_out"},
}

func TestUIMessageCatalogueMatchesUsage(t *testing.T) {
	used := map[string]bool{}
	tplKey := regexp.MustCompile(`\bt "([a-z0-9_.-]+)"`)
	err := fs.WalkDir(uiAssets, "webui/templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(uiAssets, p)
		for _, m := range tplKey.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = true
			if _, ok := messagesEN[m[1]]; !ok {
				t.Errorf("%s uses unknown message key %q", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	goKey := regexp.MustCompile(`msg\("([a-z0-9_.-]*[a-z0-9_-])"`)
	files, _ := filepath.Glob("ui*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range goKey.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = true
			if _, ok := messagesEN[m[1]]; !ok {
				t.Errorf("%s uses unknown message key %q", f, m[1])
			}
		}
		// Keys passed to errorPage and similar as bare strings: "err.x" and "login.x".
		for _, m := range regexp.MustCompile(`"((?:err|login|filter|dashboard|activity|board|sprints|epics|issue)\.[a-z_]+)"`).FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = true
			if _, ok := messagesEN[m[1]]; !ok {
				t.Errorf("%s uses unknown message key %q", f, m[1])
			}
		}
	}
	for fam, vals := range dynamicMessageFamilies {
		for _, v := range vals {
			k := fam + v
			if _, ok := messagesEN[k]; !ok {
				t.Errorf("missing message %q", k)
			}
			used[k] = true
		}
	}
	for _, lt := range LinkTypes {
		for _, dir := range []string{"outbound", "inbound"} {
			k := "link." + lt + "." + dir
			if _, ok := messagesEN[k]; !ok {
				t.Errorf("missing message %q", k)
			}
			used[k] = true
		}
	}
	for k := range messagesEN {
		if !used[k] {
			t.Errorf("message %q is never used", k)
		}
	}
	for k, v := range messagesEN {
		if v == "" {
			t.Errorf("message %q is empty", k)
		}
	}
}
