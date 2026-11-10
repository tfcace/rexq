package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// A version 1 store migrates to version 2: the agent column is added, every
// existing row is Claude's (""), and nothing else changes.
func TestMigrateVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	writeV1Store(t, path)
	before := dumpV1(t, path)
	s := must(Open(path))
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 2 {
		t.Fatalf("user_version %d %v", v, err)
	}
	var agents []string
	rows, err := s.db.Query(`SELECT agent FROM items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		rows.Scan(&a)
		agents = append(agents, a)
	}
	rows.Close()
	if !reflect.DeepEqual(agents, []string{"", "", "", ""}) {
		t.Errorf("agents %q, want every row Claude's", agents)
	}
	all := must(s.query(`SELECT ` + columns + ` FROM items`))
	for _, it := range all {
		if it.Agent != "" {
			t.Errorf("item %d agent %q", it.ID, it.Agent)
		}
	}
	s.Close()
	if after := dumpV1(t, path); !reflect.DeepEqual(after, before) {
		t.Errorf("migration changed rows\nbefore %v\n after %v", before, after)
	}
	// The items table is the version 1 table plus the column, last.
	db, _ := sql.Open("sqlite", "file:"+path)
	defer db.Close()
	var cols []string
	r, _ := db.Query(`SELECT name FROM pragma_table_info('items') ORDER BY cid`)
	for r.Next() {
		var n string
		r.Scan(&n)
		cols = append(cols, n)
	}
	r.Close()
	if cols[len(cols)-1] != "agent" || len(cols) != 23 {
		t.Errorf("columns %q", cols)
	}
	// The query a version 1 panel loads with now fails.
	if _, err := db.Query(`SELECT COALESCE((SELECT updated_at FROM prompts WHERE prompts.claude_session = items.claude_session), 0) FROM items`); err == nil ||
		!strings.Contains(err.Error(), "no such table: prompts") {
		t.Errorf("a version 1 query still works: %v", err)
	}
}

// Many processes opening an old store at once migrate it once.
func TestMigrateConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	writeV1Store(t, path)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			s.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A store that a newer rexq migrated past this one is refused, also when
// the newer one moved it on while this one waited for the lock.
func TestMigrateRefusesANewerStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	s := must(Open(path))
	s.db.Exec("PRAGMA user_version = 3")
	s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("opened a newer store")
	}
}

func TestPiItemRoundTrip(t *testing.T) {
	s := openTemp(t)
	it := sample("01a1c30f-ea0a", "e1a4472c:1234", 0)
	it.Agent = "pi"
	id := must2(s.Insert(it))
	got, found, err := s.Item(id)
	if err != nil || !found || got.Agent != "pi" {
		t.Fatalf("got %+v %v %v", got, found, err)
	}
	if list := must(s.List("")); len(list) != 1 || list[0].Agent != "pi" {
		t.Fatalf("list %+v", list)
	}
	// InsertFromPane and InsertBackfill keep it too.
	it2 := sample("01a1c30f-ea0b", "x", 0)
	it2.Agent = "pi"
	must2(s.InsertFromPane(it2))
	it3 := sample("01a1c30f-ea0c", "y", 0)
	it3.Agent = "pi"
	if _, err := s.InsertBackfill("01a1c30f-ea0c", []Item{it3}, it3.CreatedAt); err != nil {
		t.Fatal(err)
	}
	for _, x := range must(s.List("")) {
		if x.Agent != "pi" {
			t.Errorf("%s: agent %q", x.ClaudeSession, x.Agent)
		}
	}
}
