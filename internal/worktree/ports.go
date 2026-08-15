package worktree

import (
	"fmt"
	"net"
)

// MaxSlots bounds concurrent worktrees. Each band below is 32 wide
// (BASE+1 .. BASE+32); slot 0 is the main checkout and is never allocated
// from these bands — it keeps docker-compose.yml's own defaults.
const MaxSlots = 32

// Port bands, one per service, each clear of: the repo's own slot-0
// defaults (8080, 5173), the production stack's fixed ports (8180 — see
// docker-compose.prod.yml, never touched by this package), and everything
// else this machine had listening when the bands were chosen. The last two
// digits of every allocated port equal the slot, so `docker ps` / `lsof`
// output is readable at a glance.
//
// The 4600/8600/4700 bands are no longer allocated — they were the dev and
// test broker client/monitor ports, freed when the broker left the stack
// (docs/QUEUE-MIGRATION-PLAN.md §8). They are kept clear of the two
// remaining bands in case a future service wants them.
const (
	bandHarnessHTTP = 8700 // dev HARNESS_HTTP_PORT / host DEEPSEEK_HTTP_ADDR; also serves /mcp
	bandVite        = 5700 // web/vite.config.ts dev server port
)

// Ports is one worktree slot's full port allocation.
type Ports struct {
	HarnessHTTP int `xml:"harness-http" json:"harness_http"`
	Vite        int `xml:"vite-dev" json:"vite_dev"`
}

// PortsForSlot computes slot's allocation. It is a pure function of slot —
// see docs/WORKTREES.md's "the footprint is enumerable" rationale. Existing
// allocated worktrees need no migration: each keeps its slot number and
// simply gets this shorter port list; the registry entry stores Ports as
// JSON, and encoding/json ignores the three now-unknown keys on read
// (docs/QUEUE-MIGRATION-PLAN.md §8).
func PortsForSlot(slot int) Ports {
	return Ports{
		HarnessHTTP: bandHarnessHTTP + slot,
		Vite:        bandVite + slot,
	}
}

// list returns the two ports in a fixed order, for probing and printing.
func (p Ports) list() []int {
	return []int{p.HarnessHTTP, p.Vite}
}

// probeFree attempts to bind every port in p on 127.0.0.1 and releases each
// immediately. It reports the first one already held by something outside
// the registry, so allocation can skip to the next slot instead of handing
// out a port that only fails later, deep inside `docker compose up`.
func (p Ports) probeFree() error {
	for _, port := range p.list() {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return fmt.Errorf("port %d is already in use: %w", port, err)
		}
		l.Close()
	}
	return nil
}
