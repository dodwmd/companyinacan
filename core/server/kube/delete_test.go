package kube

import (
	"context"
	"strings"
	"testing"
)

func TestDeleteUsesBackgroundPropagation(t *testing.T) {
	var got rec
	c := server(t, 200, `{}`, &got)
	if err := c.Delete(context.Background(), "Job", "mod-a", "seed"); err != nil {
		t.Fatal(err)
	}
	if got.method != "DELETE" || !strings.Contains(got.query, "propagationPolicy=Background") {
		t.Fatalf("a deleted Job would leave its pods running: %+v", got)
	}
}
