package installer

import (
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// mergedValues returns the chart's values.yaml defaults overlaid with the can-core supplied values,
// as the renderer saw them. Errors are not reported: the render that precedes this call has already
// read and parsed the same file.
func mergedValues(chartDir string, overrides map[string]any) map[string]any {
	defaults := map[string]any{}
	if root, err := filepath.EvalSymlinks(chartDir); err == nil {
		if b, err := readInside(root, filepath.Join(root, "values.yaml")); err == nil {
			_ = yaml.Unmarshal(b, &defaults)
		}
	}
	return deepMerge(defaults, overrides)
}
