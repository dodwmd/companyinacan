package people

import (
	"context"
	"encoding/json"
	"errors"
)

// fakeEngine is an in-memory approval engine.
type fakeEngine struct {
	reqs      []ApprovalRequest
	out, by   string
	cancelled []string
	failReq   bool
}

func (f *fakeEngine) Request(_ context.Context, r ApprovalRequest) (string, error) {
	if f.failReq {
		return "", errors.New("engine down")
	}
	f.reqs = append(f.reqs, r)
	return "apr_fake", nil
}

func (f *fakeEngine) Status(context.Context, string) (ApprovalStatus, error) {
	out := f.out
	if out == "" {
		out = "pending"
	}
	return ApprovalStatus{Outcome: out, DecidedBy: f.by}, nil
}

func (f *fakeEngine) Cancel(_ context.Context, ref string) error {
	f.cancelled = append(f.cancelled, ref)
	return nil
}

func jsonString(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
