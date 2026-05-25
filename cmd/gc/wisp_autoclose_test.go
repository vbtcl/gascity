package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestWispAutocloseClosesOpenMolecule(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "work item"})                                // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Errorf("stdout = %q, want auto-close message", stdout.String())
	}

	b, err := store.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("wisp Status = %q, want %q", b.Status, "closed")
	}
}

func TestWispAutocloseClosesMetadataAttachedMolecule(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "work item",
		Metadata: map[string]string{"molecule_id": "gc-2"},
	}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp", Type: "molecule"}) // gc-2
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Fatalf("stdout = %q, want metadata auto-close message", stdout.String())
	}

	b, err := store.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Fatalf("metadata-attached molecule status = %q, want closed", b.Status)
	}
}

func TestWispAutocloseClosesAttachedMoleculeDescendants(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "work item",
		Metadata: map[string]string{"molecule_id": "gc-2"},
	}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "molecule root", Type: "molecule"})        // gc-2
	_, _ = store.Create(beads.Bead{Title: "step", Type: "task", ParentID: "gc-2"})   // gc-3
	_, _ = store.Create(beads.Bead{Title: "nested", Type: "task", ParentID: "gc-3"}) // gc-4
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Fatalf("stdout = %q, want metadata auto-close message", stdout.String())
	}
	for _, id := range []string{"gc-2", "gc-3", "gc-4"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, b.Status)
		}
	}
}

func TestWispAutocloseChecksDescendantsWhenAttachedRootAlreadyClosed(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "work item",
		Metadata: map[string]string{"molecule_id": "gc-2"},
	}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "molecule root", Type: "molecule"})      // gc-2
	_, _ = store.Create(beads.Bead{Title: "step", Type: "task", ParentID: "gc-2"}) // gc-3
	_ = store.Close("gc-2")
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Fatalf("stdout = %q, want auto-close message for descendant cleanup", stdout.String())
	}
	child, err := store.Get("gc-3")
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != "closed" {
		t.Fatalf("descendant status = %q, want closed", child.Status)
	}
}

func TestWispAutocloseSkipsAlreadyClosed(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "work item"})                                // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-2")
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if stdout.String() != "" {
		t.Errorf("already-closed wisp should produce no output, got %q", stdout.String())
	}
}

func TestWispAutocloseSkipsNonMoleculeChildren(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "convoy", Type: "convoy"})               // gc-1
	_, _ = store.Create(beads.Bead{Title: "task", Type: "task", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if stdout.String() != "" {
		t.Errorf("non-molecule children should produce no output, got %q", stdout.String())
	}

	b, _ := store.Get("gc-2")
	if b.Status != "open" {
		t.Errorf("non-molecule child Status = %q, want %q", b.Status, "open")
	}
}

func TestWispAutocloseNoChildren(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "lone bead"}) // gc-1
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if stdout.String() != "" {
		t.Errorf("no-children bead should produce no output, got %q", stdout.String())
	}
}

func TestWispAutocloseMultipleMolecules(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "work item"})                                  // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp A", Type: "molecule", ParentID: "gc-1"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "wisp B", Type: "molecule", ParentID: "gc-1"}) // gc-3
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	out := stdout.String()
	if !strings.Contains(out, "gc-2") || !strings.Contains(out, "gc-3") {
		t.Errorf("should close both wisps, got %q", out)
	}

	for _, id := range []string{"gc-2", "gc-3"} {
		b, _ := store.Get(id)
		if b.Status != "closed" {
			t.Errorf("wisp %s Status = %q, want %q", id, b.Status, "closed")
		}
	}
}

func TestWispAutocloseBeadNotFound(t *testing.T) {
	store := beads.NewMemStore()

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "nonexistent", &stdout)

	if stdout.String() != "" {
		t.Errorf("missing bead should produce no output, got %q", stdout.String())
	}
}

func TestWispAutocloseUsesHookBeadWithoutParentLookup(t *testing.T) {
	mem := beads.NewMemStore()
	parent, _ := mem.Create(beads.Bead{Title: "work item"})
	_, _ = mem.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: parent.ID})
	_ = mem.Close(parent.ID)

	store := parentGetFailsStore{Store: mem, parentID: parent.ID}

	var stdout bytes.Buffer
	doWispAutocloseWithHookBead(store, parent, parent.ID, &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Fatalf("stdout = %q, want auto-close message from hook bead payload", stdout.String())
	}
}

func TestWispAutocloseHookBeadParsesBDObject(t *testing.T) {
	parent, ok := wispAutocloseHookBead(strings.NewReader(`{"id":"gc-1","title":"closed","issue_type":"task"}`), "gc-1")
	if !ok {
		t.Fatal("wispAutocloseHookBead did not parse bd hook object")
	}
	if parent.ID != "gc-1" || parent.Title != "closed" {
		t.Fatalf("parent = %#v, want parsed hook bead", parent)
	}
}

func TestWispAutocloseHookBeadSkipsMismatchedID(t *testing.T) {
	if parent, ok := wispAutocloseHookBead(strings.NewReader(`{"id":"gc-2","title":"other"}`), "gc-1"); ok {
		t.Fatalf("wispAutocloseHookBead = %#v, true; want fallback on mismatched id", parent)
	}
}

type parentGetFailsStore struct {
	beads.Store
	parentID string
}

func (s parentGetFailsStore) Get(id string) (beads.Bead, error) {
	if id == s.parentID {
		return beads.Bead{}, fmt.Errorf("unexpected parent lookup for %s", id)
	}
	return s.Store.Get(id)
}
