package main

import (
	"encoding/json"
	"net/http"
)

// Names for chairs.
//
// A seat is a session key, and a session key is deliberately nobody: it is
// minted for one table and thrown away. But the people at the table arrived
// through a conversation, and a person looking at a chair wants to see who is
// sitting in it, not a number.
//
// The mapping comes in two halves that meet here. This process learns which
// Bison Relay identity spoke for each seat - the sender of the seat's own
// funding and bond announcements, the two messages only their owner ever sends.
// The host, which is the only party that can ask Bison Relay anything, says
// what those identities are called. Neither half is worth anything alone, and
// the joined result is worth exactly one thing: a label.
//
// Labels, not identity. Nothing that moves money reads a name, a seat that
// cannot be resolved is shown by its number, and a name proves nothing about
// who holds the key. The route is host-only - it sits off the page allowlist -
// because what things are called is the host's knowledge to give, not a page's
// to set.

// handleNamesSet takes the host's word for what identities are called.
func (p *plugin) handleNamesSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		// Names maps a Bison Relay identity, as hex, to what the host
		// calls it.
		Names map[string]string `json:"names"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	p.tables.mu.Lock()
	// Merged rather than replaced: mints happen per panel, and a second
	// panel naming fewer people must not unname the rest.
	for uid, name := range req.Names {
		if name == "" {
			delete(p.tables.names, uid)
			continue
		}
		p.tables.names[uid] = name
	}
	n := len(p.tables.names)
	p.tables.mu.Unlock()

	writeJSON(w, map[string]any{"known": n})
}
