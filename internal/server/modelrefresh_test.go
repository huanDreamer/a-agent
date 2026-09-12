package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/store"
)

// ageModels backdates a provider's fetched rows, which is the only way to make a
// list stale without waiting for a TTL to pass.
func ageModels(t *testing.T, st store.Store, providerID string, age time.Duration) {
	t.Helper()
	stamp := time.Now().UTC().Add(-age).Format("2006-01-02 15:04:05")
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE llm_models SET fetched_at = ? WHERE provider_id = ?`, stamp, providerID); err != nil {
		t.Fatalf("age models of %s: %v", providerID, err)
	}
}

// fetchedModels seeds rows that look like the result of a successful fetch.
func fetchedModels(t *testing.T, st store.Store, providerID string, ids ...string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		if err := st.UpsertModel(ctx, store.Model{
			ProviderID: providerID, ModelID: id, DisplayName: id,
			Capabilities: store.Capabilities{store.CapChat}, Enabled: true, Source: modelSourceFetched,
		}); err != nil {
			t.Fatalf("upsert model %s: %v", id, err)
		}
	}
	// ReplaceFetchedModels is what the refresh actually uses, so the rows are
	// rewritten through it to look exactly like a real fetch.
	rows := make([]store.Model, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, store.Model{ProviderID: providerID, ModelID: id})
	}
	if err := st.ReplaceFetchedModels(ctx, providerID, rows); err != nil {
		t.Fatalf("replace fetched models: %v", err)
	}
}

/* ------------------------------------------------------------- staleness --- */

func TestProviderModelsStale(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	cases := []struct {
		name   string
		ttl    time.Duration
		models []store.Model
		want   bool
	}{
		{
			name:   "never fetched",
			ttl:    time.Hour,
			models: nil,
			want:   true,
		},
		{
			name: "only hand-added models count as never fetched",
			ttl:  time.Hour,
			models: []store.Model{{
				ModelID: "mine", Source: "user", FetchedAt: ptrTime(now),
			}},
			want: true,
		},
		{
			name: "fetched just now",
			ttl:  time.Hour,
			models: []store.Model{{
				ModelID: "a", Source: modelSourceFetched, FetchedAt: ptrTime(now),
			}},
			want: false,
		},
		{
			name: "fetched longer ago than the ttl",
			ttl:  time.Hour,
			models: []store.Model{{
				ModelID: "a", Source: modelSourceFetched, FetchedAt: ptrTime(now.Add(-2 * time.Hour)),
			}},
			want: true,
		},
		{
			name: "the newest row decides",
			ttl:  time.Hour,
			models: []store.Model{
				{ModelID: "old", Source: modelSourceFetched, FetchedAt: ptrTime(now.Add(-48 * time.Hour))},
				{ModelID: "new", Source: modelSourceFetched, FetchedAt: ptrTime(now.Add(-time.Minute))},
			},
			want: false,
		},
		{
			name: "a zero ttl never expires",
			ttl:  0,
			models: []store.Model{{
				ModelID: "a", Source: modelSourceFetched, FetchedAt: ptrTime(now.Add(-1000 * time.Hour)),
			}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerModelsStale(tc.models, tc.ttl); got != tc.want {
				t.Errorf("providerModelsStale = %v, want %v", got, tc.want)
			}
		})
	}

	// The stored shape is what matters in the end: a row written by a real fetch
	// must read as fresh, and the same row backdated must read as stale.
	st := catalogStore(t,
		[]store.Provider{providerRow("p", true, "http://127.0.0.1:1/v1")},
		nil,
	)
	fetchedModels(t, st, "p", "m1", "m2")
	rows, err := st.ListModels(ctx, "p")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if providerModelsStale(rows, 24*time.Hour) {
		t.Errorf("a row written by a fetch must be fresh: %+v", rows[0].FetchedAt)
	}
	ageModels(t, st, "p", 48*time.Hour)
	rows, err = st.ListModels(ctx, "p")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if !providerModelsStale(rows, 24*time.Hour) {
		t.Error("a backdated row must be stale")
	}
}

func ptrTime(v time.Time) *time.Time { return &v }

/* --------------------------------------------------------- one provider --- */

func TestRefreshProviderModels_StoresAndInfersCapabilities(t *testing.T) {
	mock := newMockProvider(t, "chat-model", "text-embedding-3", "gpt-4o")
	st := catalogStore(t,
		[]store.Provider{providerRow("p", true, "PLACEHOLDER")},
		// A model with a name the operator typed and a capability they corrected.
		[]store.Model{{
			ProviderID: "p", ModelID: "gpt-4o", DisplayName: "我的名字",
			Capabilities: store.Capabilities{store.CapChat}, Enabled: false,
		}},
	)
	if err := st.UpsertProvider(context.Background(), store.Provider{
		ID: "p", Name: "p", BaseURL: mock.srv.URL, Kind: providerKindOpenAI,
		Source: store.SourceUser, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	withKey(t, st, "p", "sk-p")

	p, err := st.GetProvider(context.Background(), "p")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	saved, err := refreshProviderModels(context.Background(), st, zap.NewNop(), p)
	if err != nil {
		t.Fatalf("refreshProviderModels: %v", err)
	}
	if len(saved) != 3 {
		t.Fatalf("saved = %d models, want 3", len(saved))
	}

	byID := map[string]store.Model{}
	for _, m := range saved {
		byID[m.ModelID] = m
	}
	// A refresh learns ids and nothing else: the name and the enabled flag the
	// operator set survive it, and the capabilities are inferred and then left
	// for the operator to correct.
	if got := byID["gpt-4o"]; got.DisplayName != "我的名字" || got.Enabled {
		t.Errorf("gpt-4o = %+v, want the stored name and the disabled flag preserved", got)
	}
	if got := byID["text-embedding-3"]; got.Has(store.CapChat) {
		t.Errorf("text-embedding-3 = %+v, want it inferred as an embedding model", got.Capabilities)
	}
	if got := byID["chat-model"]; !got.Has(store.CapChat) {
		t.Errorf("chat-model = %+v, want it inferred as a chat model", got.Capabilities)
	}
	// The fetch reached the provider, with its key.
	if len(mock.paths()) == 0 || !strings.HasSuffix(mock.paths()[0], "/models") {
		t.Errorf("requests = %v, want a /models call", mock.paths())
	}
	if auths := mock.auths(); len(auths) == 0 || auths[0] != "Bearer sk-p" {
		t.Errorf("authorization = %v, want the provider's key", auths)
	}
}

func TestRefreshProviderModels_FailureKeepsTheStoredList(t *testing.T) {
	mock := newMockProvider(t, "m1")
	mock.modelsStatus = http.StatusUnauthorized
	st := catalogStore(t,
		[]store.Provider{providerRow("p", true, mock.srv.URL)},
		[]store.Model{chatModelRow("p", "kept")},
	)
	withKey(t, st, "p", "sk-p")
	p, err := st.GetProvider(context.Background(), "p")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}

	if _, err := refreshProviderModels(context.Background(), st, zap.NewNop(), p); err == nil {
		t.Fatal("want an error when the provider answers 401")
	}
	// The stored list must survive: empty is a worse answer than stale.
	rows, err := st.ListModels(context.Background(), "p")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(rows) != 1 || rows[0].ModelID != "kept" {
		t.Errorf("stored models = %+v, want the previous list untouched", rows)
	}
}

/* ------------------------------------------------------- the whole pass --- */

// TestRefreshProviderSet_OneFailureDoesNotAbortThePass is the requirement that a
// broken provider records its error and nothing else stops.
func TestRefreshProviderSet_OneFailureDoesNotAbortThePass(t *testing.T) {
	good := newMockProvider(t, "a", "b")
	good2 := newMockProvider(t, "c")
	bad := newMockProvider(t)
	bad.modelsStatus = http.StatusBadGateway

	st := catalogStore(t,
		[]store.Provider{
			providerRow("good", true, good.srv.URL),
			providerRow("good2", true, good2.srv.URL),
			providerRow("bad", true, bad.srv.URL),
		},
		[]store.Model{chatModelRow("bad", "stale-but-kept")},
	)
	for _, id := range []string{"good", "good2", "bad"} {
		withKey(t, st, id, "sk-"+id)
	}

	targets, err := refreshTargets(context.Background(), st)
	if err != nil {
		t.Fatalf("refreshTargets: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("targets = %d, want 3 enabled providers with a key", len(targets))
	}

	results := refreshProviderSet(context.Background(), st, zap.NewNop(), targets)
	if len(results) != 3 {
		t.Fatalf("results = %d, want one per provider", len(results))
	}
	byID := map[string]ModelRefreshResult{}
	for _, r := range results {
		byID[r.ProviderID] = r
	}
	if !byID["good"].OK || byID["good"].ModelsCount != 2 {
		t.Errorf("good = %+v, want ok with 2 models", byID["good"])
	}
	if !byID["good2"].OK || byID["good2"].ModelsCount != 1 {
		t.Errorf("good2 = %+v, want ok with 1 model; a failure must not stop the pass", byID["good2"])
	}
	if byID["bad"].OK || byID["bad"].Error == "" {
		t.Errorf("bad = %+v, want a failure with its reason", byID["bad"])
	}
	if !strings.Contains(byID["bad"].Error, "models") {
		t.Errorf("error = %q, want it to name the failing call", byID["bad"].Error)
	}

	// The failure is recorded on the provider; the success clears any previous
	// error.
	badRow, err := st.GetProvider(context.Background(), "bad")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if badRow.LastError == "" {
		t.Error("a failed refresh must record last_error on the provider")
	}
	goodRow, err := st.GetProvider(context.Background(), "good")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if goodRow.LastError != "" {
		t.Errorf("last_error = %q, want it cleared after a success", goodRow.LastError)
	}
	// And the failed provider's stored list is still there.
	kept, err := st.ListModels(context.Background(), "bad")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(kept) != 1 {
		t.Errorf("models = %d, want the previous list kept", len(kept))
	}
}

func TestRefreshTargets_SkipsDisabledAndKeylessProviders(t *testing.T) {
	mock := newMockProvider(t, "m")
	st := catalogStore(t,
		[]store.Provider{
			providerRow("on", true, mock.srv.URL),
			providerRow("off", false, mock.srv.URL),
			providerRow("nokey", true, mock.srv.URL),
		},
		nil,
	)
	withKey(t, st, "on", "sk-on")
	withKey(t, st, "off", "sk-off") // has a key but is disabled

	targets, err := refreshTargets(context.Background(), st)
	if err != nil {
		t.Fatalf("refreshTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].ID != "on" {
		t.Errorf("targets = %+v, want only the enabled provider with a key", targets)
	}
}

// TestRefreshStaleModels_OnlyRefreshesWhatIsStale is the startup-pass decision
// end to end: a fresh provider is left alone, a stale one is refetched, and a
// keyless one is never contacted.
func TestRefreshStaleModels_OnlyRefreshesWhatIsStale(t *testing.T) {
	stale := newMockProvider(t, "s1", "s2")
	fresh := newMockProvider(t, "f1")
	keyless := newMockProvider(t, "k1")

	st := catalogStore(t,
		[]store.Provider{
			providerRow("stale", true, stale.srv.URL),
			providerRow("fresh", true, fresh.srv.URL),
			providerRow("keyless", true, keyless.srv.URL),
		},
		nil,
	)
	withKey(t, st, "stale", "sk-stale")
	withKey(t, st, "fresh", "sk-fresh")

	fetchedModels(t, st, "stale", "old-model")
	ageModels(t, st, "stale", 48*time.Hour)
	fetchedModels(t, st, "fresh", "f1")

	results := RefreshStaleModels(context.Background(), st, zap.NewNop(), 24*time.Hour)
	if len(results) != 1 {
		t.Fatalf("results = %+v, want only the stale provider", results)
	}
	// ModelsCount counts what is stored afterwards: the two just fetched plus
	// "old-model", which the store keeps — ReplaceFetchedModels upserts and never
	// deletes, so a model the provider stops listing stays in the catalog (it may
	// be one the operator added by hand, and the store cannot tell them apart).
	if results[0].ProviderID != "stale" || !results[0].OK || results[0].ModelsCount != 3 {
		t.Fatalf("result = %+v, want stale refreshed (2 fetched, 1 kept)", results[0])
	}
	if len(fresh.paths()) != 0 {
		t.Errorf("a fresh provider must not be refetched: %v", fresh.paths())
	}
	if len(keyless.paths()) != 0 {
		t.Errorf("a provider with no key must not be contacted: %v", keyless.paths())
	}

	// The refreshed list is persisted, and it is what the catalog now serves.
	rows, err := st.ListModels(context.Background(), "stale")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("stored models = %d, want the 2 fetched ones plus the kept one", len(rows))
	}
	seen := map[string]bool{}
	for _, m := range rows {
		seen[m.ModelID] = true
		if m.Source != modelSourceFetched {
			t.Errorf("%s has source %q, want a fetched row", m.ModelID, m.Source)
		}
	}
	if !seen["s1"] || !seen["s2"] {
		t.Errorf("fetched models missing: %+v", rows)
	}
	cat := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{TTL: 24 * time.Hour}).Catalog(context.Background())
	summary := providerByID(t, cat, "stale")
	if summary.Stale || summary.LastFetchedAt == nil {
		t.Errorf("stale = %+v, want it fresh now with a fetch time", summary)
	}
}

// TestRefreshStaleModels_FailureDoesNotAbort is the same requirement at the pass
// level: one broken provider, the rest still refreshed.
func TestRefreshStaleModels_FailureDoesNotAbort(t *testing.T) {
	broken := newMockProvider(t)
	broken.modelsStatus = http.StatusUnauthorized
	ok := newMockProvider(t, "ok-model")

	st := catalogStore(t,
		[]store.Provider{
			providerRow("broken", true, broken.srv.URL),
			providerRow("ok", true, ok.srv.URL),
		},
		nil,
	)
	withKey(t, st, "broken", "sk-broken")
	withKey(t, st, "ok", "sk-ok")

	results := RefreshStaleModels(context.Background(), st, zap.NewNop(), time.Hour)
	if len(results) != 2 {
		t.Fatalf("results = %+v, want both providers attempted", results)
	}
	byID := map[string]ModelRefreshResult{}
	for _, r := range results {
		byID[r.ProviderID] = r
	}
	if byID["broken"].OK {
		t.Error("the broken provider must be reported as failed")
	}
	if !byID["ok"].OK || byID["ok"].ModelsCount != 1 {
		t.Errorf("ok = %+v, want it refreshed despite the other failure", byID["ok"])
	}
	row, err := st.GetProvider(context.Background(), "broken")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if row.LastError == "" {
		t.Error("last_error must be recorded for the failed provider")
	}
}

func TestRefreshStaleModels_ZeroTTLDoesNothing(t *testing.T) {
	mock := newMockProvider(t, "m")
	st := catalogStore(t, []store.Provider{providerRow("p", true, mock.srv.URL)}, nil)
	withKey(t, st, "p", "sk-p")

	if got := RefreshStaleModels(context.Background(), st, zap.NewNop(), 0); got != nil {
		t.Errorf("results = %+v, want nothing refreshed when the TTL is zero", got)
	}
	if len(mock.paths()) != 0 {
		t.Errorf("requests = %v, want none", mock.paths())
	}
}

/* -------------------------------------------------------------- the route --- */

func TestHandleRefreshAllModels(t *testing.T) {
	ok := newMockProvider(t, "a", "b")
	broken := newMockProvider(t)
	broken.modelsStatus = http.StatusForbidden

	h, _ := newCatalogHarness(t, func(st store.Store) {
		ctx := context.Background()
		for _, p := range []store.Provider{
			{ID: "ok", Name: "OK", BaseURL: ok.srv.URL, Kind: providerKindOpenAI, Source: store.SourceUser, Enabled: true},
			{ID: "broken", Name: "Broken", BaseURL: broken.srv.URL, Kind: providerKindOpenAI, Source: store.SourceUser, Enabled: true},
			{ID: "nokey", Name: "NoKey", BaseURL: "http://127.0.0.1:1/v1", Kind: providerKindOpenAI, Source: store.SourceUser, Enabled: true},
		} {
			if err := st.UpsertProvider(ctx, p); err != nil {
				t.Fatalf("upsert provider: %v", err)
			}
		}
		if err := st.SetProviderKey(ctx, "ok", "sk-ok"); err != nil {
			t.Fatalf("set key: %v", err)
		}
		if err := st.SetProviderKey(ctx, "broken", "sk-broken"); err != nil {
			t.Fatalf("set key: %v", err)
		}
	}, func(st store.Store) ModelBuilder {
		return NewCatalogModelBuilder(st, nil, ModelBuilderOptions{})
	})

	resp := h.postJSON(t, "/api/llm/models/refresh-all", nil)
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		t.Fatalf("status = %d, want 200 (a failing provider is data, not an API error)", resp.StatusCode)
	}
	var body struct {
		Results []ModelRefreshResult `json:"results"`
	}
	decode(t, resp, &body)

	if len(body.Results) != 2 {
		t.Fatalf("results = %+v, want the two providers with a key", body.Results)
	}
	byID := map[string]ModelRefreshResult{}
	for _, r := range body.Results {
		byID[r.ProviderID] = r
	}
	if !byID["ok"].OK || byID["ok"].ModelsCount != 2 {
		t.Errorf("ok = %+v", byID["ok"])
	}
	if byID["broken"].OK || byID["broken"].Error == "" {
		t.Errorf("broken = %+v, want ok=false with the reason", byID["broken"])
	}
	if _, found := byID["nokey"]; found {
		t.Error("a provider with no key must not be refreshed")
	}

	// The refreshed list reaches the catalog the console reads — this is what
	// makes 刷新全部 visible in the composer too.
	var cat struct {
		Models    []ModelChoice    `json:"models"`
		Providers []ProviderChoice `json:"providers"`
	}
	h.getJSON(t, "/api/chat/models", http.StatusOK, &cat)
	if len(cat.Models) != 2 {
		t.Fatalf("models = %d, want the 2 just fetched", len(cat.Models))
	}
	if cat.Models[0].Provider != "ok" {
		t.Errorf("model = %+v, want it from the refreshed provider", cat.Models[0])
	}
	summary := providerByID(t, ModelCatalog{Providers: cat.Providers}, "ok")
	if summary.Stale || summary.LastFetchedAt == nil {
		t.Errorf("summary = %+v, want it fresh with a fetch time", summary)
	}
	if got := providerByID(t, ModelCatalog{Providers: cat.Providers}, "broken"); got.LastError == "" {
		t.Error("the failure must be visible on the provider summary")
	}
}
