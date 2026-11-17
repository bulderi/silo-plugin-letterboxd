package provider

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Entries older than the window are dropped on the next write, and an account
// keeps at most maxLoggedEntries, so the stored state stays small.
func TestAccountStorePrunesEntries(t *testing.T) {
	clock := newManualClock(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	store := newMemStore()
	accounts := newAccountStore(store, clock.Now)
	ctx := t.Context()

	if err := accounts.addEntry(ctx, "Sample", loggedEntry{LID: "old", ID: "e-old", Logged: clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	clock.Add(diaryEntryWindow + time.Second)
	for i := range maxLoggedEntries + 5 {
		entry := loggedEntry{LID: fmt.Sprintf("f%d", i), ID: fmt.Sprintf("e%d", i), Logged: clock.Now().Unix()}
		if err := accounts.addEntry(ctx, "sample", entry); err != nil {
			t.Fatal(err)
		}
	}

	var stored map[string]accountState
	if err := json.Unmarshal(store.values[accountsKey], &stored); err != nil {
		t.Fatal(err)
	}
	entries := stored["sample"].Entries
	if len(entries) != maxLoggedEntries {
		t.Fatalf("stored %d entries, want %d", len(entries), maxLoggedEntries)
	}
	for _, e := range entries {
		if e.LID == "old" || e.ID == "e0" {
			t.Fatalf("kept %+v, want the expired and the oldest entries dropped", e)
		}
	}
	if _, found, err := accounts.newestEntry(ctx, "SAMPLE", fmt.Sprintf("f%d", maxLoggedEntries+4)); err != nil || !found {
		t.Fatalf("newest entry not found (%v)", err)
	}
}
