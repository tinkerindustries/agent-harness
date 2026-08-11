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
// defaults (4222, 8222, 8080, 4422, 5173), the production stack's
// fixed ports (4522, 8522, 8180 — see docker-compose.prod.yml, never
// touched by this package), and everything else this machine had listening
// when the bands were chosen. The last two digits of every allocated port
// equal the slot, so `docker ps` / `lsof` output is readable at a glance.
const (
	bandNATSClient  = 4600 // dev NATS_CLIENT_PORT
	bandNATSMonitor = 8600 // dev NATS_MONITOR_PORT
	bandHarnessHTTP = 8700 // dev HARNESS_HTTP_PORT / host DEEPSEEK_HTTP_ADDR; also serves /mcp
	bandTestNATS    = 4700 // docker-compose.test.yml HARNESS_TEST_NATS_PORT
	bandVite        = 5700 // web/vite.config.ts dev server port
)

// Ports is one worktree slot's full port allocation.
type Ports struct {
	NATSClient  int `xml:"nats-client" json:"nats_client"`
	NATSMonitor int `xml:"nats-monitor" json:"nats_monitor"`
	HarnessHTTP int `xml:"harness-http" json:"harness_http"`
	TestNATS    int `xml:"test-nats" json:"test_nats"`
	Vite        int `xml:"vite-dev" json:"vite_dev"`
}

// PortsForSlot computes slot's allocation. It is a pure function of slot —
// see docs/WORKTREES.md's "the footprint is enumerable" rationale.
func PortsForSlot(slot int) Ports {
	return Ports{
		NATSClient:  bandNATSClient + slot,
		NATSMonitor: bandNATSMonitor + slot,
		HarnessHTTP: bandHarnessHTTP + slot,
		TestNATS:    bandTestNATS + slot,
		Vite:        bandVite + slot,
	}
}

// list returns the five ports in a fixed order, for probing and printing.
func (p Ports) list() []int {
	return []int{p.NATSClient, p.NATSMonitor, p.HarnessHTTP, p.TestNATS, p.Vite}
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
