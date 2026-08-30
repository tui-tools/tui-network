package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-network/internal/network"
)

// checkTimeout bounds the read. Loading the model shells out to networkctl,
// resolvectl and ip, and a machine whose network service is wedged must not
// hang a non-interactive check forever.
const checkTimeout = 30 * time.Second

// checkReport is what --check prints: the model the backend parsed, plus the
// counts a test can assert on without walking the whole structure.
//
// It is a report of the read path only. --check never builds and never runs a
// mutation: the whole point is that it is safe to run anywhere, including in
// CI against a production-shaped machine.
type checkReport struct {
	Tool    string `json:"tool"`
	Version string `json:"version"`
	Backend string `json:"backend"`
	// Describe is the backend's own one-line summary, which is where the demo
	// backend says it is a demo.
	Describe string `json:"describe"`
	// Running and ResolvedRunning report which daemons answered.
	Running         bool `json:"running"`
	ResolvedRunning bool `json:"resolvedRunning"`
	// ForeignManager names another network manager found on the machine.
	ForeignManager string `json:"foreignManager,omitempty"`
	// Links, Managed, Routes and ConfigFiles are the totals across the model.
	// Managed is the count a smoke test uses to tell a networkd machine from
	// a NetworkManager one without parsing the whole model.
	Links       int `json:"links"`
	Managed     int `json:"managed"`
	Routes      int `json:"routes"`
	ConfigFiles int `json:"configFiles"`
	// Compat is what the backend version probe found. It is reported rather
	// than asserted: an untested version is a fact about the machine, not a
	// failure of the read path.
	Compat compat.Result `json:"compat"`
	// Model is the parsed state in full.
	Model network.Model `json:"model"`
}

// runCheck exercises the backend's real read path and prints the parsed model
// as JSON. It returns an error when the backend cannot be read, which main
// turns into a non-zero exit — so a caller can treat the exit code alone as
// the verdict.
//
// A machine running NetworkManager is not a failure: networkctl still lists
// the links, every one of them comes back unmanaged, and the report says so.
// That is the read path working, and it is what the smoke test asserts there.
func runCheck(backend network.Backend, backendCompat compat.Result,
	out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	model, err := backend.Load(ctx)
	if err != nil {
		return fmt.Errorf("%s backend read failed: %w", backend.Name(), err)
	}

	report := checkReport{
		Tool:            toolName,
		Version:         version,
		Backend:         backend.Name(),
		Describe:        backend.Describe(),
		Running:         model.Running,
		ResolvedRunning: model.ResolvedRunning,
		ForeignManager:  model.ForeignManager,
		Links:           len(model.Links),
		Routes:          len(model.Routes),
		ConfigFiles:     len(model.ConfigFiles),
		Compat:          backendCompat,
		Model:           model,
	}
	for _, link := range model.Links {
		if link.Managed {
			report.Managed++
		}
	}

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}
