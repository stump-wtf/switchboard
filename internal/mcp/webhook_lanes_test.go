package mcp

// fakeStore stub for the ADR-0025 grant read. Like the other routing stubs it models nothing: the
// exclusive-delivery scope read is a join, and every assertion about it runs against the real store.

import "context"

func (f *fakeStore) EndpointScopeQueues(_ context.Context, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}
