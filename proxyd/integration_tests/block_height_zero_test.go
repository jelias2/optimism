package integration_tests

import (
	"context"
	"net/http"
	"os"
	"path"
	"testing"
	"time"

	"github.com/ethereum-optimism/optimism/proxyd"
	sw "github.com/ethereum-optimism/optimism/proxyd/pkg/avg-sliding-window"
	ms "github.com/ethereum-optimism/optimism/proxyd/tools/mockserver/handler"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/stretchr/testify/require"
)

type bhZeroNodeContext struct {
	// NOTE: maybe reuse the node context from consensus test here
	backend      *proxyd.Backend   // this is the actual backend impl in proxyd
	mockBackend  *MockBackend      // this is the fake backend that we can use to mock responses
	handler      *ms.MockedHandler // this is where we control the state of mocked responses
	bhZeroWindow *sw.AvgSlidingWindow
	clock        *sw.AdjustableClock // this is where we control backend time
}

const (
	SlidingWindowLength     = 10 * time.Second
	SlidingWindowBucketSize = 1 * time.Second
)

// ts is a convenient method that must parse a time.Time from a string in format `"2006-01-02 15:04:05"`
func ts(s string) time.Time {
	t, err := time.Parse(time.DateTime, s)
	if err != nil {
		panic(err)
	}
	return t
}

func setupBlockHeightZero(t *testing.T) (map[string]*bhZeroNodeContext, *proxyd.BackendGroup, *ProxydHTTPClient, func(), proxyd.TOMLDuration) {
	// setup mock servers
	node1 := NewMockBackend(nil)
	node2 := NewMockBackend(nil)

	dir, err := os.Getwd()
	require.NoError(t, err)

	responses := path.Join(dir, "testdata/block_height_zero_responses.yml")

	h1 := ms.MockedHandler{
		Overrides:    []*ms.MethodTemplate{},
		Autoload:     true,
		AutoloadFile: responses,
	}
	h2 := ms.MockedHandler{
		Overrides:    []*ms.MethodTemplate{},
		Autoload:     true,
		AutoloadFile: responses,
	}

	require.NoError(t, os.Setenv("NODE1_URL", node1.URL()))
	require.NoError(t, os.Setenv("NODE2_URL", node2.URL()))

	node1.SetHandler(http.HandlerFunc(h1.Handler))
	node2.SetHandler(http.HandlerFunc(h2.Handler))

	// setup proxyd
	config := ReadConfig("block_height_zero")
	banPeriod := config.BackendGroups["node"].ConsensusBanPeriod
	svr, shutdown, err := proxyd.Start(config)
	require.NoError(t, err)

	// expose the proxyd client
	client := NewProxydClient("http://127.0.0.1:8545")

	// expose the backend group
	bg := svr.BackendGroups["node"]
	require.NotNil(t, bg)
	require.NotNil(t, bg.Consensus)
	require.Equal(t, 2, len(bg.Backends)) // should match config

	now := ts("2023-04-21 15:00:00")

	clock := sw.NewAdjustableClock(now)
	sw1 := sw.NewSlidingWindow(
		sw.WithWindowLength(SlidingWindowLength),
		sw.WithBucketSize(SlidingWindowBucketSize),
		sw.WithClock(clock))

	bg.Backends[0].Override(proxyd.WithBlockHeightZeroSlidingWindow(sw1))
	bg.Backends[1].Override(proxyd.WithBlockHeightZeroSlidingWindow(sw1))

	require.Equal(t, bg.Backends[0].GetBlockHeightZeroSlidingWindowLength(),
		time.Duration(config.Backends["node1"].BlockHeightZeroWindowLength))

	require.Equal(t, bg.Backends[1].GetBlockHeightZeroSlidingWindowLength(),
		time.Duration(config.Backends["node2"].BlockHeightZeroWindowLength))

	// convenient mapping to access the nodes, and sliding windows by name
	nodes := map[string]*bhZeroNodeContext{
		"node1": {
			mockBackend:  node1,
			backend:      bg.Backends[0],
			handler:      &h1,
			bhZeroWindow: bg.Backends[0].GetBlockHeightZeroSlidingWindow(),
		},
		"node2": {
			mockBackend:  node2,
			backend:      bg.Backends[1],
			handler:      &h2,
			bhZeroWindow: bg.Backends[1].GetBlockHeightZeroSlidingWindow(),
			clock:        clock,
		},
	}

	return nodes, bg, client, shutdown, banPeriod
}

func TestBlockHeightZero(t *testing.T) {
	nodes, bg, _, shutdown, banPeriod := setupBlockHeightZero(t)
	defer nodes["node1"].mockBackend.Close()
	defer nodes["node2"].mockBackend.Close()
	defer shutdown()

	ctx := context.Background()

	// poll for updated consensus
	update := func() {
		for _, be := range bg.Backends {
			bg.Consensus.UpdateBackend(ctx, be)
		}
		bg.Consensus.UpdateBackendGroupConsensus(ctx)
	}

	// ts is a convenient method that must parse a time.Time from a string in format `"2006-01-02 15:04:05"`
	ts := func(s string) time.Time {
		t, err := time.Parse(time.DateTime, s)
		if err != nil {
			panic(err)
		}
		return t
	}

	addTimeToBackend := func(node string, nodes map[string]*bhZeroNodeContext, bg *proxyd.BackendGroup, ts time.Duration) {
		// updatedBackends := []*proxyd.Backend{}
		for i, b := range bg.Backends {
			if b.Name == node {
				// Get the mock backend and set the clock
				// clock := mockBackend.backend.GetBlockHeightZeroSlidingWindow()
				clock := b.GetBlockHeightZeroSlidingWindow()
				clock.SetTime(clock.Now().Add(ts))
				b.SetBlockHeightZeroSlidingWindow(clock)
				bg.Backends[i] = b

				// Update the mock backend mapping
				// nodes[node] = mockBackend
				// updatedBackends = append(updatedBackends, mockBackend.backend)
			}
		}
		// copy(bg.Backends, updatedBackends)
	}
	// convenient methods to manipulate state and mock responses
	reset := func() {
		for _, node := range nodes {
			node.handler.ResetOverrides()
			node.mockBackend.Reset()
		}
		bg.Consensus.ClearListeners()
		bg.Consensus.Reset()
		b1 := nodes["node1"]
		b2 := nodes["node2"]

		require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend))
		require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend))

		require.Zero(t, nodes["node2"].backend.GetBlockHeightZeroSlidingWindowCount())
		require.Zero(t, nodes["node1"].backend.GetBlockHeightZeroSlidingWindowCount())

		now := ts("2023-04-21 15:04:00")
		clock := sw.NewAdjustableClock(now)
		b1.bhZeroWindow = sw.NewSlidingWindow(
			sw.WithWindowLength(60*time.Second),
			sw.WithBucketSize(time.Second),
			sw.WithClock(clock))

		b2.bhZeroWindow = sw.NewSlidingWindow(
			sw.WithWindowLength(60*time.Second),
			sw.WithBucketSize(time.Second),
			sw.WithClock(clock))

		// addTimeToBackend("node1", nodes, bg, 23*time.Second)
		bg.Backends[0].SetBlockHeightZeroSlidingWindow(b1.bhZeroWindow)
		bg.Backends[1].SetBlockHeightZeroSlidingWindow(b2.bhZeroWindow)
	}

	override := func(node string, method string, block string, response string) {
		if _, ok := nodes[node]; !ok {
			t.Fatalf("node %s does not exist in the nodes map", node)
		}
		nodes[node].handler.AddOverride(&ms.MethodTemplate{
			Method:   method,
			Block:    block,
			Response: response,
		})
	}

	overrideBlock := func(node string, blockRequest string, blockResponse string) {
		override(node,
			"eth_getBlockByNumber",
			blockRequest,
			buildResponse(map[string]string{
				"number": blockResponse,
				"hash":   "hash_" + blockResponse,
			}))
	}

	overridePeerCount := func(node string, count int) {
		override(node, "net_peerCount", "", buildResponse(hexutil.Uint64(count).String()))
	}

	t.Run("Test Backend BlockHeight Zero Does not if the infractions occur outside window", func(t *testing.T) {
		reset()
		overrideBlock("node1", "latest", "0x0")
		for i := 0; i < 6; i++ {
			update()
			// Expect the infractions to be recorded, in the sliding window qty
			require.Equal(t, uint(1), nodes["node1"].backend.GetBlockHeightZeroSlidingWindowCount())
			require.Equal(t, float64(1), nodes["node1"].backend.GetBlockHeightZeroSlidingWindowAvg())
			require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend))
			require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend))
			addTimeToBackend("node1", nodes, bg, 221*time.Second)
		}
		require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend))
		require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend))
	})

	t.Run("Test Backend BlockHeight Zero Activates at after 5 infratctions", func(t *testing.T) {
		reset()
		overrideBlock("node1", "latest", "0x0")
		for i := 0; i < 10; i++ {
			update()
			require.Equal(t, uint(i+1), nodes["node1"].backend.GetBlockHeightZeroSlidingWindowCount())
			if i < 6 {
				require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend))
				require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend))
			} else {
				require.True(t, bg.Consensus.IsBanned(nodes["node1"].backend))
				require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend))

			}
			addTimeToBackend("node1", nodes, bg, 3*time.Second)
		}
		require.True(t, bg.Consensus.IsBanned(nodes["node1"].backend))
		require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend))
	})

	t.Run("Test backend does not activate if the ban period expiries", func(t *testing.T) {
		reset()

		delay := nodes["node2"].backend.GetBlockHeightZeroSlidingWindowLength() / 2
		overrideBlock("node2", "latest", "0x0")
		for i := 0; i < 10; i++ {
			// require.Equal(t, uint(i), nodes["node2"].backend.GetBlockHeightZeroSlidingWindowCount())
			require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend), "Expected Node2 Not to be Banned on iteration %d. NOTE: THIS PASS WILL NOT PASS IN DEBUG", i)
			require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend), "Expected Node1 Not to be Banned on interation %d. NOTE: THIS PASS WILL NOT PASS IN DEBUG", i)
			update()
			time.Sleep(time.Duration(banPeriod))
		}
		time.Sleep(delay)
		require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend))
		require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend))
	})

	t.Run("Test Backend BlockHeight Activates then Deactivates", func(t *testing.T) {
		reset()

		overrideBlock("node2", "latest", "0x0")
		for i := 0; i < 10; i++ {
			update()
			if i > 4 {
				require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend), "Expected node1 not to be banned on iteration %d. NOTE: THIS PASS WILL NOT PASS IN DEBUG", i)
				require.True(t, bg.Consensus.IsBanned(nodes["node2"].backend), "Expected node2 not to be banned on iteration %d. NOTE: THIS PASS WILL NOT PASS IN DEBUG", i)
			} else {
				require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend), "Expected node1 Not to be Banned on iteration %d. NOTE: THIS PASS WILL NOT PASS IN DEBUG", i)
				require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend), "Expected node2 Not to be Banned on iteration %d. NOTE: THIS PASS WILL NOT PASS IN DEBUG", i)
			}
		}
		overrideBlock("node2", "latest", "0x1")
		bg.Consensus.Unban(nodes["node2"].backend)
		require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend))
		require.False(t, bg.Consensus.IsBanned(nodes["node2"].backend))
	})
	t.Run("initial consensus", func(t *testing.T) {
		reset()

		// unknown consensus at init
		require.Equal(t, "0x0", bg.Consensus.GetLatestBlockNumber().String())

		// first poll
		update()

		// as a default we use:
		// - latest at 0x101 [257]
		// - safe at 0xe1 [225]
		// - finalized at 0xc1 [193]

		// consensus at block 0x101
		require.Equal(t, "0x101", bg.Consensus.GetLatestBlockNumber().String())
		require.Equal(t, "0xe1", bg.Consensus.GetSafeBlockNumber().String())
		require.Equal(t, "0xc1", bg.Consensus.GetFinalizedBlockNumber().String())
	})

	t.Run("prevent using a backend with low peer count", func(t *testing.T) {
		reset()
		overridePeerCount("node1", 0)
		update()

		consensusGroup := bg.Consensus.GetConsensusGroup()
		require.NotContains(t, consensusGroup, nodes["node1"].backend)
		require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend))
		require.Equal(t, 1, len(consensusGroup))
	})

	t.Run("prevent using a backend lagging behind", func(t *testing.T) {
		reset()
		// node2 is 8+1 blocks ahead of node1 (0x101 + 8+1 = 0x10a)
		overrideBlock("node2", "latest", "0x10a")
		update()

		// since we ignored node1, the consensus should be at 0x10a
		require.Equal(t, "0x10a", bg.Consensus.GetLatestBlockNumber().String())
		require.Equal(t, "0xe1", bg.Consensus.GetSafeBlockNumber().String())
		require.Equal(t, "0xc1", bg.Consensus.GetFinalizedBlockNumber().String())

		consensusGroup := bg.Consensus.GetConsensusGroup()
		require.NotContains(t, consensusGroup, nodes["node1"].backend)
		require.False(t, bg.Consensus.IsBanned(nodes["node1"].backend))
		require.Equal(t, 1, len(consensusGroup))
	})

}
