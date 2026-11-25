package installer

import (
	"fmt"
	"text/template"
	"text/template/parse"
	"time"
)

// tick is called once per `range` iteration and per `include`, in every template of the chart. It is
// the CPU bound of the renderer: Go templates cannot be interrupted, so every loop body is
// instrumented to count its own iterations and to stop the render when the budget, the renderer's
// deadline or the caller's context is exhausted. (A nested range over two 200k-element lists would
// otherwise never finish.)
func (st *renderState) tick() (string, error) {
	st.ticks++
	if st.ticks > maxTicks {
		return "", fmt.Errorf("rendering exceeded %d loop iterations and includes", maxTicks)
	}
	if st.ticks%256 == 0 {
		if time.Now().After(st.deadline) {
			return "", fmt.Errorf("rendering took longer than %s", renderTimeout)
		}
		if st.ctx != nil && st.ctx.Err() != nil {
			return "", st.ctx.Err()
		}
	}
	return "", nil
}

// instrumentRanges inserts `{{ tick }}` as the first action of every range body of every template
// in the set (including define blocks and files not rendered directly).
func instrumentRanges(rt *template.Template, st *renderState) error {
	probe, err := rt.New("__tick").Parse("{{ tick }}")
	if err != nil {
		return err
	}
	if probe.Tree == nil || len(probe.Root.Nodes) != 1 {
		return fmt.Errorf("renderer: cannot build the tick action")
	}
	action := probe.Root.Nodes[0]
	for _, t := range rt.Templates() {
		if t.Tree == nil || t.Name() == "__tick" {
			continue
		}
		walk(t.Root, action)
	}
	return nil
}

func walk(n parse.Node, tick parse.Node) {
	switch x := n.(type) {
	case *parse.ListNode:
		if x == nil {
			return
		}
		for _, c := range x.Nodes {
			walk(c, tick)
		}
	case *parse.IfNode:
		walk(x.List, tick)
		walk(x.ElseList, tick)
	case *parse.WithNode:
		walk(x.List, tick)
		walk(x.ElseList, tick)
	case *parse.RangeNode:
		if x.List != nil {
			x.List.Nodes = append([]parse.Node{tick.Copy()}, x.List.Nodes...)
			walk(x.List, tick)
		}
		walk(x.ElseList, tick)
	}
}
