package main

import (
	"encoding/json"
	"os"
	"sync"
)

// The NodeEx Connect app runs this program and reads what it does from
// standard output, one JSON object per line, when it is given -events. The
// human-readable messages still go to standard error.
var (
	eventsOn bool
	eventsMu sync.Mutex
)

// emit writes one event when -events is on.
func emit(event string, fields map[string]any) {
	if !eventsOn {
		return
	}
	m := map[string]any{"event": event}
	for k, v := range fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	eventsMu.Lock()
	defer eventsMu.Unlock()
	os.Stdout.Write(append(b, '\n'))
}
