package mcp

// tools/call tests for the SPEC-0024 notify-hook verbs over the real SDK client and server, against
// an in-memory hook store and an SSRF validator with an injected resolver (no real DNS). The
// DB-backed ownership suite is tenant_isolation_notify_hooks_test.go.
//
// Governing: SPEC-0024 REQ-2 "Management Verbs", REQ-3 "Target Validation (SSRF Guard)", REQ-4,
// REQ-11 (no query string or secret in any response).

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/push"
	"github.com/stump-wtf/switchboard/internal/store"
)

// fakeHookStore is an in-memory NotifyHookStore with the real store's ownership and ceiling rules.
type fakeHookStore struct {
	mu       sync.Mutex
	hooks    map[string]store.NotifyHook
	secrets  map[string]string
	n        int
	noCipher bool
}

func newFakeHookStore() *fakeHookStore {
	return &fakeHookStore{hooks: map[string]store.NotifyHook{}, secrets: map[string]string{}}
}

func (f *fakeHookStore) CreateNotifyHook(_ context.Context, endpointID, url string, queues []string, ignorePresence bool, secret string, max int) (store.NotifyHook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noCipher {
		return store.NotifyHook{}, store.ErrSecretCipherRequired
	}
	used := 0
	for _, h := range f.hooks {
		if h.EndpointID == endpointID {
			used++
		}
	}
	if max <= 0 || used >= max {
		return store.NotifyHook{}, store.ErrCeilingExceeded
	}
	f.n++
	h := store.NotifyHook{
		ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.n), EndpointID: endpointID, URL: url,
		Queues: queues, IgnorePresence: ignorePresence, Enabled: true, CreatedAt: time.Now(),
	}
	f.hooks[h.ID] = h
	f.secrets[h.ID] = secret
	return h, nil
}

func (f *fakeHookStore) ListNotifyHooks(_ context.Context, endpointID string) ([]store.NotifyHook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.NotifyHook
	for _, h := range f.hooks {
		if h.EndpointID == endpointID {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeHookStore) RotateNotifyHookSecret(_ context.Context, id, endpointID, newSecret string, grace time.Duration) (store.NotifyHook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.hooks[id]
	if !ok || h.EndpointID != endpointID {
		return store.NotifyHook{}, store.ErrNotFound
	}
	now := time.Now()
	exp := now.Add(grace)
	h.RotatedAt, h.PrevSecretExpiresAt = &now, &exp
	if h.DisabledReason != nil && *h.DisabledReason == store.NotifyHookDisabledFailures {
		h.Enabled, h.DisabledReason, h.ConsecutiveFailures = true, nil, 0
	}
	f.hooks[id] = h
	f.secrets[id] = newSecret
	return h, nil
}

func (f *fakeHookStore) DeleteNotifyHook(_ context.Context, id, endpointID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.hooks[id]
	if !ok || h.EndpointID != endpointID {
		return store.ErrNotFound
	}
	delete(f.hooks, id)
	delete(f.secrets, id)
	return nil
}

func (f *fakeHookStore) get(id string) (store.NotifyHook, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hooks[id], f.secrets[id]
}

func (f *fakeHookStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hooks)
}

// hookResolver answers fixed addresses and counts lookups, so a test can prove none happened.
type hookResolver struct {
	mu     sync.Mutex
	byHost map[string][]string
	calls  int
}

func (r *hookResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	var out []net.IPAddr
	for _, s := range r.byHost[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func (r *hookResolver) lookups() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newHookResolver() *hookResolver {
	return &hookResolver{byHost: map[string][]string{
		"dispatch.example.com": {"93.184.216.34"},
		"internal.example.com": {"10.0.0.5"},
		"harness.local":        {"127.0.0.1"},
	}}
}

var allNotifyHookVerbs = NotifyHookVerbs()

// notifyHookSession vends an endpoint with verbs on queues inbox+reviews, installs cfg (unless nil)
// and returns a connected session.
func notifyHookSession(t *testing.T, ctx context.Context, verbs []string, cfg *NotifyHookConfig) *sdk.ClientSession {
	t.Helper()
	f := newFakeStore()
	token := vend(t, f, defaultTestSlug, []string{"inbox", "reviews"}, verbs)
	ts, h := newTestServerHandler(t, f)
	if cfg != nil {
		h.SetNotifyHooks(*cfg)
	}
	cs, err := connect(t, ctx, ts.URL+"/mcp/"+defaultTestSlug, token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func hookConfig(st NotifyHookStore, res push.Resolver, max int, opts ...push.Option) *NotifyHookConfig {
	return &NotifyHookConfig{Store: st, Validator: push.New(append(opts, push.WithResolver(res))...), Max: max}
}

func TestNotifyHookVerbsAdvertisementAndGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := newFakeHookStore()
	cs := notifyHookSession(t, ctx, []string{"list_todos", "list_notify_hooks"}, hookConfig(st, newHookResolver(), 5))

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		if notifyHookVerbs[tool.Name] {
			names = append(names, tool.Name)
		}
	}
	if want := []string{"list_notify_hooks"}; !slices.Equal(names, want) {
		t.Fatalf("advertised notify-hook tools = %v, want %v", names, want)
	}
	// Scenario "Endpoint without the verb": forbidden, nothing stored, no lookup made.
	res := newHookResolver()
	cs2 := notifyHookSession(t, ctx, []string{"list_todos"}, hookConfig(st, res, 5))
	callErr(t, ctx, cs2, "create_notify_hook", map[string]any{"url": "https://dispatch.example.com/sb"}, codeForbidden)
	if st.count() != 0 || res.lookups() != 0 {
		t.Fatalf("ungranted create stored %d hooks and made %d lookups", st.count(), res.lookups())
	}
}

// Scenario "Not in the default grant": the notify-hook verbs are in no default verb set.
func TestNotifyHookVerbsNotInDefaultGrant(t *testing.T) {
	all := AllVerbs()
	for _, v := range NotifyHookVerbs() {
		if slices.Contains(all, v) || slices.Contains(DrainVerbs(), v) || slices.Contains(WebhookVerbs(), v) {
			t.Errorf("%s is in a default verb set", v)
		}
	}
}

func TestNotifyHookVerbsUnavailableWithoutConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs := notifyHookSession(t, ctx, allNotifyHookVerbs, nil)
	callErr(t, ctx, cs, "create_notify_hook", map[string]any{"url": "https://dispatch.example.com/sb"}, codeUnavailable)
	callErr(t, ctx, cs, "list_notify_hooks", map[string]any{}, codeUnavailable)

	// A store with no encryption key refuses too, with a message naming the fix.
	st := newFakeHookStore()
	st.noCipher = true
	cs2 := notifyHookSession(t, ctx, allNotifyHookVerbs, hookConfig(st, newHookResolver(), 5))
	msg := callErr(t, ctx, cs2, "create_notify_hook", map[string]any{"url": "https://dispatch.example.com/sb"}, codeUnavailable)
	if !strings.Contains(msg, "SWITCHBOARD_SECRET_ENCRYPTION_KEY") {
		t.Fatalf("no-key refusal %q does not name the fix", msg)
	}
}

// Scenario "Create reveals the secret once".
func TestCreateNotifyHookRevealsSecretOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st := newFakeHookStore()
	cs := notifyHookSession(t, ctx, allNotifyHookVerbs, hookConfig(st, newHookResolver(), 5))

	var created createNotifyHookOut
	callOK(t, ctx, cs, "create_notify_hook", map[string]any{
		"url": "https://dispatch.example.com/sb?token=s3cr3t", "queues": []string{"reviews", "reviews"}, "ignore_presence": true,
	}, &created)
	if !regexp.MustCompile(`^whsec_[A-Za-z0-9+/]{43}=$`).MatchString(created.SigningSecret) {
		t.Fatalf("signing_secret is not whsec_ + padded base64 of 32 bytes: %q", created.SigningSecret)
	}
	storedHook, storedSecret := st.get(created.HookID)
	if storedSecret != created.SigningSecret {
		t.Fatal("revealed secret differs from the stored one")
	}
	if created.URL != "https://dispatch.example.com/sb?redacted" || !slices.Equal(created.Queues, []string{"reviews"}) ||
		!created.IgnorePresence || !created.Enabled {
		t.Fatalf("create result = %+v", created)
	}
	// The receiver's full URL (query included) is what is stored for delivery.
	if got := storedHook.URL; got != "https://dispatch.example.com/sb?token=s3cr3t" {
		t.Fatalf("stored url = %q", got)
	}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "list_notify_hooks", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_notify_hooks: %v %s", err, contentText(res))
	}
	raw := rawJSONOf(t, res) + contentText(res)
	for _, leak := range []string{created.SigningSecret, "signing_secret", "s3cr3t"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("list_notify_hooks leaks %q: %s", leak, raw)
		}
	}
	var list listNotifyHooksOut
	rawUnmarshal(t, res, &list)
	if len(list.Hooks) != 1 || list.Ceiling.Max != 5 || list.Ceiling.Used != 1 || list.Hooks[0].HookID != created.HookID {
		t.Fatalf("list = %+v", list)
	}

	var rotated rotateNotifyHookOut
	callOK(t, ctx, cs, "rotate_notify_hook", map[string]any{"hook_id": created.HookID}, &rotated)
	if rotated.SigningSecret == created.SigningSecret || rotated.PreviousSecretValidUntil == "" || !rotated.Enabled {
		t.Fatalf("rotate result = %+v", rotated)
	}
	until, err := time.Parse(time.RFC3339, rotated.PreviousSecretValidUntil)
	if err != nil || until.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("previous_secret_valid_until = %q, want ~24h out", rotated.PreviousSecretValidUntil)
	}

	var deleted deleteNotifyHookOut
	callOK(t, ctx, cs, "delete_notify_hook", map[string]any{"hook_id": created.HookID}, &deleted)
	if !deleted.Deleted || st.count() != 0 {
		t.Fatalf("delete = %+v, %d hooks left", deleted, st.count())
	}
	callErr(t, ctx, cs, "delete_notify_hook", map[string]any{"hook_id": created.HookID}, codeNotFound)
	callErr(t, ctx, cs, "rotate_notify_hook", map[string]any{"hook_id": "nh_1"}, codeNotFound)
	callErr(t, ctx, cs, "delete_notify_hook", map[string]any{"hook_id": " "}, codeInvalidArgument)
}

// REQ-3 at create time, via the injected resolver.
func TestCreateNotifyHookTargetValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cases := []struct {
		name    string
		opts    []push.Option
		url     string
		ok      bool
		mention string
	}{
		{name: "private target refused", url: "https://internal.example.com/sb", mention: "private address"},
		{name: "plain http without the opt-in", url: "http://dispatch.example.com/sb", mention: "https"},
		{name: "plain http with the opt-in", opts: []push.Option{push.WithAllowHTTP(true)}, url: "http://dispatch.example.com/sb", ok: true},
		{name: "userinfo", url: "https://u:p@dispatch.example.com/sb", mention: "userinfo"},
		{name: "too long", url: "https://dispatch.example.com/" + strings.Repeat("a", 2048), mention: "limit"},
		{name: "metadata", url: "https://169.254.169.254/latest", mention: "link-local"},
		{name: "loopback", url: "https://harness.local:9443/", mention: "loopback"},
		{
			name: "same-host receiver when the operator opts in",
			opts: []push.Option{push.WithAllowCIDRs(mustPrefixes(t, "127.0.0.1/32")...), push.WithOwnListenAddrs("127.0.0.1:8080")},
			url:  "https://harness.local:9443/hook", ok: true,
		},
		{
			name: "own listen address stays refused",
			opts: []push.Option{push.WithAllowCIDRs(mustPrefixes(t, "127.0.0.1/32")...), push.WithOwnListenAddrs("127.0.0.1:8080")},
			url:  "https://harness.local:8080/hook", mention: "own listening address",
		},
		{
			name: "a broad entry does not open metadata",
			opts: []push.Option{push.WithAllowCIDRs(mustPrefixes(t, "0.0.0.0/0")...)},
			url:  "https://169.254.169.254/latest", mention: "link-local",
		},
		{
			name: "a broad entry does not open loopback",
			opts: []push.Option{push.WithAllowCIDRs(mustPrefixes(t, "0.0.0.0/0")...)},
			url:  "https://127.0.0.1/", mention: "loopback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeHookStore()
			cs := notifyHookSession(t, ctx, allNotifyHookVerbs, hookConfig(st, newHookResolver(), 5, tc.opts...))
			if tc.ok {
				var out createNotifyHookOut
				callOK(t, ctx, cs, "create_notify_hook", map[string]any{"url": tc.url}, &out)
				if st.count() != 1 {
					t.Fatalf("hook not stored")
				}
				return
			}
			msg := callErr(t, ctx, cs, "create_notify_hook", map[string]any{"url": tc.url}, codeInvalidArgument)
			if !strings.Contains(msg, tc.mention) {
				t.Fatalf("refusal %q does not mention %q", msg, tc.mention)
			}
			if st.count() != 0 {
				t.Fatalf("refused hook was stored")
			}
		})
	}
}

func TestCreateNotifyHookCeilingAndQueues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Ceiling 0: refused with ceiling_exceeded before the host is even resolved.
	st, res := newFakeHookStore(), newHookResolver()
	cs := notifyHookSession(t, ctx, allNotifyHookVerbs, hookConfig(st, res, 0))
	callErr(t, ctx, cs, "create_notify_hook", map[string]any{"url": "https://dispatch.example.com/sb"}, codeCeilingExceeded)
	if st.count() != 0 || res.lookups() != 0 {
		t.Fatalf("ceiling 0 stored %d hooks and made %d lookups", st.count(), res.lookups())
	}
	var list listNotifyHooksOut
	callOK(t, ctx, cs, "list_notify_hooks", map[string]any{}, &list)
	if list.Ceiling.Max != 0 || list.Hooks == nil {
		t.Fatalf("list at ceiling 0 = %+v", list)
	}

	// Ceiling reached.
	st2 := newFakeHookStore()
	cs2 := notifyHookSession(t, ctx, allNotifyHookVerbs, hookConfig(st2, newHookResolver(), 1))
	var out createNotifyHookOut
	callOK(t, ctx, cs2, "create_notify_hook", map[string]any{"url": "https://dispatch.example.com/a"}, &out)
	callErr(t, ctx, cs2, "create_notify_hook", map[string]any{"url": "https://dispatch.example.com/b"}, codeCeilingExceeded)

	// A queue filter outside the endpoint's scope is forbidden.
	callErr(t, ctx, cs2, "create_notify_hook", map[string]any{"url": "https://dispatch.example.com/c", "queues": []string{"payroll"}}, codeForbidden)
	callErr(t, ctx, cs2, "create_notify_hook", map[string]any{"url": "https://dispatch.example.com/c", "queues": []string{""}}, codeInvalidArgument)
}

func mustPrefixes(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	p, err := push.ParseCIDRList(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return p
}
