// Copyright 2026 Brad Knight and contributors. Licensed under Apache-2.0. See LICENSE.
//
// steam_demos_test.go - offline tests for the `steam demos` command and the
// `has_demo` field on `steam app`. Every case runs against an httptest
// server; nothing here touches the network.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mvanhorn/printing-press-library/library/media-and-entertainment/game-goat/internal/source/steam"
)

const (
	demosQueryPath      = "/IStoreQueryService/Query/v1/"
	demosSearchPath     = "/IStoreQueryService/SearchSuggestions/v1/"
	demosGetItemsPath   = "/IStoreBrowseService/GetItems/v1/"
	demosGetTagListPath = "/IStoreService/GetTagList/v1/"
)

// demosReqLog records every request path and the decoded input_json payload so a
// test can assert both the request count (budget) and the encoded filters.
type demosReqLog struct {
	mu       sync.Mutex
	counts   map[string]int
	payloads map[string][]map[string]any
}

func newDemosReqLog() *demosReqLog {
	return &demosReqLog{counts: map[string]int{}, payloads: map[string][]map[string]any{}}
}

func (l *demosReqLog) record(r *http.Request) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[r.URL.Path]++
	var p map[string]any
	if raw := r.URL.Query().Get("input_json"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	l.payloads[r.URL.Path] = append(l.payloads[r.URL.Path], p)
	return p
}

func (l *demosReqLog) count(path string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counts[path]
}

func (l *demosReqLog) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.counts {
		n += c
	}
	return n
}

func (l *demosReqLog) last(t *testing.T, path string) map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.payloads[path]
	if len(list) == 0 {
		t.Fatalf("no request recorded for %s (have %v)", path, l.counts)
	}
	return list[len(list)-1]
}

// demosIsolateEnv clears the locale env so a developer machine's STEAM_COUNTRY /
// STEAM_LANG cannot change the fixtures.
func demosIsolateEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"STEAM_COUNTRY", "ITAD_COUNTRY", "STEAM_LANG", "STEAM_STORE_BASE_URL", "STEAM_STORE_API_BASE_URL"} {
		t.Setenv(k, "")
	}
}

// withSteamHook points the per-request Steam client at srv for the test.
func withSteamHook(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := steamClientHook
	steamClientHook = func(c *steam.Client) {
		c.BaseURL = srv.URL
		c.APIBaseURL = srv.URL
	}
	t.Cleanup(func() { steamClientHook = old })
}

func demoItemJSON(appid int64, name string, parent int64) string {
	related := ""
	if parent > 0 {
		related = fmt.Sprintf(`,"related_items":{"parent_appid":%d}`, parent)
	}
	return fmt.Sprintf(`{"item_type":0,"id":%d,"appid":%d,"success":1,"name":%q,"type":1,"is_free":true%s}`, appid, appid, name, related)
}

func manyDemoItems(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, demoItemJSON(int64(900000+i), fmt.Sprintf("Demo %d", i), 0))
	}
	return out
}

func storeResponse(total, start, count int, items []string) string {
	return fmt.Sprintf(`{"response":{"metadata":{"total_matching_records":%d,"start":%d,"count":%d},"store_items":[%s]}}`, total, start, count, strings.Join(items, ","))
}

func nestedMap(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("payload missing object %q; have keys %v", k, cur)
		}
		cur = next
	}
	return cur
}

func getItemsIDs(t *testing.T, payload map[string]any) []int64 {
	t.Helper()
	raw, ok := payload["ids"].([]any)
	if !ok {
		t.Fatalf("GetItems payload missing ids: %v", payload)
	}
	ids := make([]int64, 0, len(raw))
	for _, x := range raw {
		m, ok := x.(map[string]any)
		if !ok {
			t.Fatalf("ids entry is not an object: %v", x)
		}
		ids = append(ids, int64(m["appid"].(float64)))
	}
	return ids
}

func tagGroupsFromFilters(t *testing.T, filters map[string]any) [][]int {
	t.Helper()
	raw, ok := filters["tagids_must_match"].([]any)
	if !ok {
		t.Fatalf("tagids_must_match missing from filters: %v", filters)
	}
	groups := make([][]int, 0, len(raw))
	for _, g := range raw {
		gm, ok := g.(map[string]any)
		if !ok {
			t.Fatalf("tag group is not an object: %v", g)
		}
		idsAny, ok := gm["tagids"].([]any)
		if !ok {
			t.Fatalf("tag group missing tagids: %v", gm)
		}
		ids := make([]int, 0, len(idsAny))
		for _, id := range idsAny {
			ids = append(ids, int(id.(float64)))
		}
		groups = append(groups, ids)
	}
	return groups
}

func runDemosCmd(t *testing.T, args ...string) (map[string]any, string, error) {
	t.Helper()
	flags := &rootFlags{asJSON: true}
	cmd := newSteamDemosCmd(flags)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if out.Len() == 0 {
		return nil, errOut.String(), err
	}
	var env map[string]any
	if uerr := json.Unmarshal(out.Bytes(), &env); uerr != nil {
		return nil, errOut.String(), fmt.Errorf("stdout is not JSON (%v): %q", uerr, out.String())
	}
	return env, errOut.String(), err
}

func TestDemosQueryEncodesDemoOnlyType(t *testing.T) {
	demosIsolateEnv(t)
	log := newDemosReqLog()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		if r.URL.Path != demosQueryPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fmt.Fprint(w, storeResponse(1, 0, 1, []string{demoItemJSON(600, "Demo One", 0)}))
	}))
	defer srv.Close()
	withSteamHook(t, srv)

	if _, _, err := runDemosCmd(t, "--limit", "20"); err != nil {
		t.Fatalf("steam demos: %v", err)
	}
	filters := nestedMap(t, log.last(t, demosQueryPath), "query", "filters")
	if got, want := filters["type_filters"], (map[string]any{"include_demos": true}); !reflect.DeepEqual(got, want) {
		t.Errorf("type_filters = %#v, want exactly %#v", got, want)
	}
	if filters["released_only"] != true {
		t.Errorf("released_only = %v, want true (default is released)", filters["released_only"])
	}
	if _, ok := filters["coming_soon_only"]; ok {
		t.Errorf("coming_soon_only must be absent by default, got %v", filters["coming_soon_only"])
	}
}

func TestDemosTitleDefaultsToReleasedOnly(t *testing.T) {
	demosIsolateEnv(t)
	log := newDemosReqLog()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		if r.URL.Path != demosSearchPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fmt.Fprint(w, storeResponse(1, 0, 1, []string{demoItemJSON(600, "Demo One", 0)}))
	}))
	defer srv.Close()
	withSteamHook(t, srv)

	if _, _, err := runDemosCmd(t, "--title", "portal", "--limit", "1000"); err != nil {
		t.Fatalf("steam demos --title: %v", err)
	}
	filters := nestedMap(t, log.last(t, demosSearchPath), "filters")
	if filters["released_only"] != true {
		t.Errorf("released_only = %v, want true (title path defaults to released)", filters["released_only"])
	}
	if _, ok := filters["coming_soon_only"]; ok {
		t.Errorf("coming_soon_only must be absent by default, got %v", filters["coming_soon_only"])
	}
}

func TestDemosComingSoonSwapsReleaseFilter(t *testing.T) {
	t.Run("browse path", func(t *testing.T) {
		demosIsolateEnv(t)
		log := newDemosReqLog()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			fmt.Fprint(w, storeResponse(1, 0, 1, []string{demoItemJSON(601, "Demo Two", 0)}))
		}))
		defer srv.Close()
		withSteamHook(t, srv)

		if _, _, err := runDemosCmd(t, "--coming-soon"); err != nil {
			t.Fatalf("steam demos --coming-soon: %v", err)
		}
		filters := nestedMap(t, log.last(t, demosQueryPath), "query", "filters")
		if filters["coming_soon_only"] != true {
			t.Errorf("coming_soon_only = %v, want true", filters["coming_soon_only"])
		}
		if _, ok := filters["released_only"]; ok {
			t.Errorf("released_only must be absent under --coming-soon, got %v", filters["released_only"])
		}
	})

	t.Run("title path", func(t *testing.T) {
		demosIsolateEnv(t)
		log := newDemosReqLog()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			if r.URL.Path != demosSearchPath {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			fmt.Fprint(w, storeResponse(273, 0, 1, []string{demoItemJSON(602, "Demo Three", 0)}))
		}))
		defer srv.Close()
		withSteamHook(t, srv)

		env, _, err := runDemosCmd(t, "--title", "portal", "--coming-soon", "--limit", "1000")
		if err != nil {
			t.Fatalf("steam demos --title --coming-soon: %v", err)
		}
		filters := nestedMap(t, log.last(t, demosSearchPath), "filters")
		if filters["coming_soon_only"] != true {
			t.Errorf("coming_soon_only = %v, want true", filters["coming_soon_only"])
		}
		if _, ok := filters["released_only"]; ok {
			t.Errorf("released_only must be absent under --coming-soon, got %v", filters["released_only"])
		}
		meta, _ := env["meta"].(map[string]any)
		if meta["total"] != float64(273) {
			t.Errorf("meta.total = %v, want 273 (the service total, not a filtered count)", meta["total"])
		}
	})
}

func TestDemosParentEnrichmentIsTwoRequestsPerPage(t *testing.T) {
	demosIsolateEnv(t)
	log := newDemosReqLog()
	items := []string{
		demoItemJSON(1001, "Demo A", 101),
		demoItemJSON(1002, "Demo B", 101),
		demoItemJSON(1003, "Demo C", 102),
		demoItemJSON(1004, "Demo D", 103),
		demoItemJSON(1005, "Demo E", 103),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := log.record(r)
		switch r.URL.Path {
		case demosQueryPath:
			fmt.Fprint(w, storeResponse(50, 0, 5, items))
		case demosGetItemsPath:
			ids := getItemsIDs(t, p)
			parts := make([]string, 0, len(ids))
			for _, id := range ids {
				parts = append(parts, fmt.Sprintf(`{"appid":%d,"success":1,"name":"Full game %d"}`, id, id))
			}
			fmt.Fprintf(w, `{"response":{"store_items":[%s]}}`, strings.Join(parts, ","))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withSteamHook(t, srv)

	env, _, err := runDemosCmd(t, "--limit", "20")
	if err != nil {
		t.Fatalf("steam demos: %v", err)
	}
	if got := log.count(demosQueryPath); got != 1 {
		t.Errorf("Query requests = %d, want 1", got)
	}
	if got := log.count(demosGetItemsPath); got != 1 {
		t.Errorf("GetItems requests = %d, want 1 (one lookup for every parent)", got)
	}
	if got := log.count(demosGetTagListPath); got != 0 {
		t.Errorf("GetTagList requests = %d, want 0 (SkipTagNames)", got)
	}
	if got := log.total(); got != 2 {
		t.Errorf("total requests = %d, want exactly 2", got)
	}
	wantIDs := []int64{101, 102, 103}
	if got := getItemsIDs(t, log.last(t, demosGetItemsPath)); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("GetItems ids = %v, want the 3 unique parents %v", got, wantIDs)
	}
	results, _ := env["results"].([]any)
	if len(results) != 5 {
		t.Fatalf("results = %d, want 5", len(results))
	}
	for i, r := range results {
		row := r.(map[string]any)
		want := fmt.Sprintf("Full game %d", int64([]int{101, 101, 102, 103, 103}[i]))
		if row["parent_name"] != want {
			t.Errorf("results[%d].parent_name = %v, want %q", i, row["parent_name"], want)
		}
	}
	meta, _ := env["meta"].(map[string]any)
	if meta["next_page"] != float64(2) {
		t.Errorf("meta.next_page = %v, want 2 when total > page window", meta["next_page"])
	}
}

func TestDemosParentLookupFailureDegrades(t *testing.T) {
	demosIsolateEnv(t)
	log := newDemosReqLog()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		switch r.URL.Path {
		case demosQueryPath:
			fmt.Fprint(w, storeResponse(1, 0, 1, []string{demoItemJSON(1001, "Demo A", 101)}))
		case demosGetItemsPath:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withSteamHook(t, srv)

	env, errOut, err := runDemosCmd(t, "--limit", "20")
	if err != nil {
		t.Fatalf("parent lookup failure must not fail the command: %v", err)
	}
	results, _ := env["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (rows survive)", len(results))
	}
	if _, ok := results[0].(map[string]any)["parent_name"]; ok {
		t.Errorf("parent_name must be absent when the lookup fails")
	}
	meta, _ := env["meta"].(map[string]any)
	missing, _ := meta["sources_missing"].([]any)
	found := false
	for _, m := range missing {
		if m == "steam_parent" {
			found = true
		}
	}
	if !found {
		t.Errorf("meta.sources_missing = %v, want to contain steam_parent", meta["sources_missing"])
	}
	if !strings.Contains(errOut, "steam_parent") {
		t.Errorf("stderr = %q, want a one-line warning naming steam_parent", errOut)
	}
}

func TestDemosTitleSetsTruncatedWhenTotalExceedsReturned(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		demosIsolateEnv(t)
		log := newDemosReqLog()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			if r.URL.Path != demosSearchPath {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			fmt.Fprint(w, storeResponse(1500, 0, 1000, manyDemoItems(1000)))
		}))
		defer srv.Close()
		withSteamHook(t, srv)

		env, _, err := runDemosCmd(t, "--title", "portal", "--limit", "1000")
		if err != nil {
			t.Fatalf("steam demos --title: %v", err)
		}
		meta, _ := env["meta"].(map[string]any)
		if meta["truncated"] != true {
			t.Errorf("meta.truncated = %v, want true (total 1500 > 1000 returned)", meta["truncated"])
		}
		if _, ok := meta["next_page"]; ok {
			t.Errorf("title path must not carry next_page, got %v", meta["next_page"])
		}
		if got := log.count(demosSearchPath); got != 1 {
			t.Errorf("SearchSuggestions requests = %d, want 1", got)
		}
	})
	t.Run("not truncated field present", func(t *testing.T) {
		demosIsolateEnv(t)
		log := newDemosReqLog()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			fmt.Fprint(w, storeResponse(25, 0, 25, manyDemoItems(25)))
		}))
		defer srv.Close()
		withSteamHook(t, srv)

		env, _, err := runDemosCmd(t, "--title", "portal", "--limit", "1000")
		if err != nil {
			t.Fatalf("steam demos --title: %v", err)
		}
		meta, _ := env["meta"].(map[string]any)
		val, present := meta["truncated"]
		if !present {
			t.Fatalf("meta must carry truncated on the title path even when false: %v", meta)
		}
		if val != false {
			t.Errorf("meta.truncated = %v, want false (25 of 25)", val)
		}
		if _, ok := meta["next_page"]; ok {
			t.Errorf("title path must not carry next_page, got %v", meta["next_page"])
		}
	})
}

func TestDemosTitleRejectsPage(t *testing.T) {
	demosIsolateEnv(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()
	withSteamHook(t, srv)

	flags := &rootFlags{asJSON: true}
	cmd := newSteamDemosCmd(flags)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--title", "portal", "--page", "2"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--page with --title must be a usage error")
	}
	if got := ExitCode(err); got != 2 {
		t.Fatalf("ExitCode = %d, want 2 (usage error)", got)
	}
	if !strings.Contains(err.Error(), "ignores offsets") {
		t.Errorf("error = %q, want it to explain the endpoint ignores offsets", err.Error())
	}
	if requests != 0 {
		t.Errorf("HTTP requests = %d, want 0 (rejected before any fetch)", requests)
	}
}

func TestDemosTagsAreOneGroupEach(t *testing.T) {
	const tagList = `{"response":{"tags":[{"tagid":1716,"name":"Roguelike"},{"tagid":1628,"name":"Metroidvania"}]}}`
	want := [][]int{{1716}, {1628}}

	cases := []struct {
		name string
		args []string
		path string
	}{
		{"browse repeated tags", []string{"--tag", "Roguelike", "--tag", "Metroidvania"}, demosQueryPath},
		{"browse comma tags", []string{"--tag", "Roguelike,Metroidvania"}, demosQueryPath},
		{"title comma tags", []string{"--title", "x", "--tag", "Roguelike,Metroidvania"}, demosSearchPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			demosIsolateEnv(t)
			log := newDemosReqLog()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.record(r)
				switch r.URL.Path {
				case demosGetTagListPath:
					fmt.Fprint(w, tagList)
				case demosQueryPath:
					fmt.Fprint(w, storeResponse(0, 0, 0, nil))
				case demosSearchPath:
					fmt.Fprint(w, storeResponse(0, 0, 0, nil))
				case demosGetItemsPath:
					fmt.Fprint(w, `{"response":{"store_items":[]}}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			withSteamHook(t, srv)

			if _, _, err := runDemosCmd(t, tc.args...); err != nil {
				t.Fatalf("steam demos: %v", err)
			}
			if got := log.count(demosGetTagListPath); got != 1 {
				t.Errorf("GetTagList requests = %d, want 1 (dictionary is cached per client)", got)
			}
			payload := log.last(t, tc.path)
			var filters map[string]any
			if tc.path == demosQueryPath {
				filters = nestedMap(t, payload, "query", "filters")
			} else {
				filters = nestedMap(t, payload, "filters")
			}
			if got := tagGroupsFromFilters(t, filters); !reflect.DeepEqual(got, want) {
				t.Errorf("tagids_must_match = %v, want exactly one group per tag %v", got, want)
			}
		})
	}
}

func TestSteamAppRowHasDemo(t *testing.T) {
	const tagList = `{"response":{"tags":[{"tagid":19,"name":"Action"}]}}`
	cases := []struct {
		name       string
		storeItem  string
		wantHasDem bool
	}{
		{
			name:       "demo links present",
			storeItem:  `{"item_type":0,"id":379720,"appid":379720,"success":1,"name":"DOOM","type":0,"related_items":{"demo_appid":[479030],"demos":[{"appid":479030}]}}`,
			wantHasDem: true,
		},
		{
			name:       "no demo links",
			storeItem:  `{"item_type":0,"id":1199790,"appid":1199790,"success":1,"name":"DOOM Eternal","type":0}`,
			wantHasDem: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			demosIsolateEnv(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case demosGetItemsPath:
					fmt.Fprintf(w, `{"response":{"store_items":[%s]}}`, tc.storeItem)
				case demosGetTagListPath:
					fmt.Fprint(w, tagList)
				case "/appreviews/379720", "/appreviews/1199790":
					fmt.Fprint(w, `{"success":1,"query_summary":{"review_score_desc":"Very Positive","total_positive":9,"total_negative":1,"total_reviews":10,"review_score":9}}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			withSteamHook(t, srv)

			var appid string
			if tc.wantHasDem {
				appid = "379720"
			} else {
				appid = "1199790"
			}
			flags := &rootFlags{asJSON: true}
			cmd := newSteamAppCmd(flags)
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{appid})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("steam app: %v (stderr %s)", err, errOut.String())
			}
			var env struct {
				Results []map[string]any `json:"results"`
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("stdout is not JSON: %v (%s)", err, out.String())
			}
			if len(env.Results) != 1 {
				t.Fatalf("results = %d, want 1", len(env.Results))
			}
			row := env.Results[0]
			val, present := row["has_demo"]
			if !present {
				t.Fatalf("has_demo must always be present, got row %v", row)
			}
			if val != tc.wantHasDem {
				t.Errorf("has_demo = %v, want %v", val, tc.wantHasDem)
			}
		})
	}
}
