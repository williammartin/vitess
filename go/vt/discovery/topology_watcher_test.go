/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package discovery

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"vitess.io/vitess/go/test/utils"

	"vitess.io/vitess/go/vt/logutil"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/memorytopo"
)

func checkOpCounts(t *testing.T, prevCounts, deltas map[string]int64) map[string]int64 {
	t.Helper()
	newCounts := topologyWatcherOperations.Counts()
	for key, prevVal := range prevCounts {
		delta, ok := deltas[key]
		if !ok {
			delta = 0
		}
		newVal, ok := newCounts[key]
		if !ok {
			newVal = 0
		}

		assert.Equal(t, newVal, prevVal+delta, "expected %v to increase by %v, got %v -> %v", key, delta, prevVal, newVal)
	}
	return newCounts
}

func checkChecksum(t *testing.T, tw *TopologyWatcher, want uint32) {
	t.Helper()
	assert.Equal(t, want, tw.TopoChecksum())
}

func TestStartAndCloseTopoWatcher(t *testing.T) {
	ctx := utils.LeakCheckContext(t)

	ts := memorytopo.NewServer(ctx, "aa")
	defer ts.Close()
	fhc := NewFakeHealthCheck(nil)
	defer fhc.Close()
	topologyWatcherOperations.ZeroAll()
	tw := NewCellTabletsWatcher(context.Background(), ts, fhc, nil, "aa", 100*time.Microsecond, true, 5)

	done := make(chan bool, 3)
	result := make(chan bool, 1)
	go func() {
		// We wait for the done channel three times since we execute three
		// topo-watcher actions (Start, Stop and Wait), once we have read
		// from the done channel three times we know we have completed all
		// the actions, the test is then successful.
		// Each action has a one-second timeout after which the test will be
		// marked as failed.
		for i := 0; i < 3; i++ {
			select {
			case <-time.After(1 * time.Second):
				close(result)
				return
			case <-done:
				break
			}
		}
		result <- true
	}()

	tw.Start()
	done <- true

	// This sleep gives enough time to the topo-watcher to do 10 iterations
	// The topo-watcher's refresh interval is set to 100 microseconds.
	time.Sleep(1 * time.Millisecond)

	tw.Stop()
	done <- true

	tw.wg.Wait()
	done <- true

	_, ok := <-result
	if !ok {
		t.Fatal("timed out")
	}
}

func TestCellTabletsWatcher(t *testing.T) {
	checkWatcher(t, true)
}

func TestCellTabletsWatcherNoRefreshKnown(t *testing.T) {
	checkWatcher(t, false)
}

func checkWatcher(t *testing.T, refreshKnownTablets bool) {
	ctx := utils.LeakCheckContext(t)

	ts := memorytopo.NewServer(ctx, "aa")
	defer ts.Close()
	fhc := NewFakeHealthCheck(nil)
	defer fhc.Close()
	logger := logutil.NewMemoryLogger()
	topologyWatcherOperations.ZeroAll()
	counts := topologyWatcherOperations.Counts()
	tw := NewCellTabletsWatcher(context.Background(), ts, fhc, nil, "aa", 10*time.Minute, refreshKnownTablets, 5)

	counts = checkOpCounts(t, counts, map[string]int64{})
	checkChecksum(t, tw, 0)

	// Add a tablet to the topology.
	tablet := &topodatapb.Tablet{
		Alias: &topodatapb.TabletAlias{
			Cell: "aa",
			Uid:  0,
		},
		Hostname: "host1",
		PortMap: map[string]int32{
			"vt": 123,
		},
		Keyspace: "keyspace",
		Shard:    "shard",
	}
	if err := ts.CreateTablet(context.Background(), tablet); err != nil {
		t.Fatalf("CreateTablet failed: %v", err)
	}
	tw.loadTablets()
	counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 1, "AddTablet": 1})
	checkChecksum(t, tw, 3238442862)

	// Check the tablet is returned by GetAllTablets().
	allTablets := fhc.GetAllTablets()
	key := TabletToMapKey(tablet)
	if _, ok := allTablets[key]; !ok || len(allTablets) != 1 || !proto.Equal(allTablets[key], tablet) {
		t.Errorf("fhc.GetAllTablets() = %+v; want %+v", allTablets, tablet)
	}

	// Add a second tablet to the topology.
	tablet2 := &topodatapb.Tablet{
		Alias: &topodatapb.TabletAlias{
			Cell: "aa",
			Uid:  2,
		},
		Hostname: "host2",
		PortMap: map[string]int32{
			"vt": 789,
		},
		Keyspace: "keyspace",
		Shard:    "shard",
	}
	if err := ts.CreateTablet(context.Background(), tablet2); err != nil {
		t.Fatalf("CreateTablet failed: %v", err)
	}
	tw.loadTablets()

	// If refreshKnownTablets is disabled, only the new tablet is read
	// from the topo
	if refreshKnownTablets {
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 2, "AddTablet": 1})
	} else {
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 1, "AddTablet": 1})
	}
	checkChecksum(t, tw, 2762153755)

	// Check the new tablet is returned by GetAllTablets().
	allTablets = fhc.GetAllTablets()
	key = TabletToMapKey(tablet2)
	if _, ok := allTablets[key]; !ok || len(allTablets) != 2 || !proto.Equal(allTablets[key], tablet2) {
		t.Errorf("fhc.GetAllTablets() = %+v; want %+v", allTablets, tablet2)
	}

	// Load the tablets again to show that when refreshKnownTablets is disabled,
	// only the list is read from the topo and the checksum doesn't change
	tw.loadTablets()
	if refreshKnownTablets {
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 2})
	} else {
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1})
	}
	checkChecksum(t, tw, 2762153755)

	// same tablet, different port, should update (previous
	// one should go away, new one be added)
	//
	// if refreshKnownTablets is disabled, this case is *not*
	// detected and the tablet remains in the topo using the
	// old key
	origTablet := tablet.CloneVT()
	origKey := TabletToMapKey(tablet)
	tablet.PortMap["vt"] = 456
	if _, err := ts.UpdateTabletFields(context.Background(), tablet.Alias, func(t *topodatapb.Tablet) error {
		t.PortMap["vt"] = 456
		return nil
	}); err != nil {
		t.Fatalf("UpdateTabletFields failed: %v", err)
	}
	tw.loadTablets()
	allTablets = fhc.GetAllTablets()
	key = TabletToMapKey(tablet)

	if refreshKnownTablets {
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 2, "ReplaceTablet": 1})

		if _, ok := allTablets[key]; !ok || len(allTablets) != 2 || !proto.Equal(allTablets[key], tablet) {
			t.Errorf("fhc.GetAllTablets() = %+v; want %+v", allTablets, tablet)
		}
		if _, ok := allTablets[origKey]; ok {
			t.Errorf("fhc.GetAllTablets() = %+v; don't want %v", allTablets, origKey)
		}
		checkChecksum(t, tw, 2762153755)
	} else {
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1})

		if _, ok := allTablets[origKey]; !ok || len(allTablets) != 2 || !proto.Equal(allTablets[origKey], origTablet) {
			t.Errorf("fhc.GetAllTablets() = %+v; want %+v", allTablets, origTablet)
		}
		if _, ok := allTablets[key]; ok {
			t.Errorf("fhc.GetAllTablets() = %+v; don't want %v", allTablets, key)
		}
		checkChecksum(t, tw, 2762153755)
	}

	// Both tablets restart on different hosts.
	// tablet2 happens to land on the host:port that tablet 1 used to be on.
	// This can only be tested when we refresh known tablets.
	if refreshKnownTablets {
		origTablet := tablet.CloneVT()
		origTablet2 := tablet2.CloneVT()
		if _, err := ts.UpdateTabletFields(context.Background(), tablet2.Alias, func(t *topodatapb.Tablet) error {
			t.Hostname = tablet.Hostname
			t.PortMap = tablet.PortMap
			tablet2 = t
			return nil
		}); err != nil {
			t.Fatalf("UpdateTabletFields failed: %v", err)
		}
		if _, err := ts.UpdateTabletFields(context.Background(), tablet.Alias, func(t *topodatapb.Tablet) error {
			t.Hostname = "host3"
			tablet = t
			return nil
		}); err != nil {
			t.Fatalf("UpdateTabletFields failed: %v", err)
		}
		tw.loadTablets()
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 2, "ReplaceTablet": 2})
		allTablets = fhc.GetAllTablets()
		key2 := TabletToMapKey(tablet2)
		if _, ok := allTablets[key2]; !ok {
			t.Fatalf("tablet was lost because it's reusing an address recently used by another tablet: %v", key2)
		}

		// Change tablets back to avoid altering later tests.
		if _, err := ts.UpdateTabletFields(context.Background(), tablet2.Alias, func(t *topodatapb.Tablet) error {
			t.Hostname = origTablet2.Hostname
			t.PortMap = origTablet2.PortMap
			tablet2 = t
			return nil
		}); err != nil {
			t.Fatalf("UpdateTabletFields failed: %v", err)
		}
		if _, err := ts.UpdateTabletFields(context.Background(), tablet.Alias, func(t *topodatapb.Tablet) error {
			t.Hostname = origTablet.Hostname
			tablet = t
			return nil
		}); err != nil {
			t.Fatalf("UpdateTabletFields failed: %v", err)
		}
		tw.loadTablets()
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 2, "ReplaceTablet": 2})
	}

	// Remove the tablet and check that it is detected as being gone.
	if err := ts.DeleteTablet(context.Background(), tablet.Alias); err != nil {
		t.Fatalf("DeleteTablet failed: %v", err)
	}
	if _, err := topo.FixShardReplication(context.Background(), ts, logger, "aa", "keyspace", "shard"); err != nil {
		t.Fatalf("FixShardReplication failed: %v", err)
	}
	tw.loadTablets()
	if refreshKnownTablets {
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 1, "RemoveTablet": 1})
	} else {
		counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "RemoveTablet": 1})
	}
	checkChecksum(t, tw, 789108290)

	allTablets = fhc.GetAllTablets()
	key = TabletToMapKey(tablet)
	if _, ok := allTablets[key]; ok || len(allTablets) != 1 {
		t.Errorf("fhc.GetAllTablets() = %+v; don't want %v", allTablets, key)
	}
	key = TabletToMapKey(tablet2)
	if _, ok := allTablets[key]; !ok || len(allTablets) != 1 || !proto.Equal(allTablets[key], tablet2) {
		t.Errorf("fhc.GetAllTablets() = %+v; want %+v", allTablets, tablet2)
	}

	// Remove the other and check that it is detected as being gone.
	if err := ts.DeleteTablet(context.Background(), tablet2.Alias); err != nil {
		t.Fatalf("DeleteTablet failed: %v", err)
	}
	if _, err := topo.FixShardReplication(context.Background(), ts, logger, "aa", "keyspace", "shard"); err != nil {
		t.Fatalf("FixShardReplication failed: %v", err)
	}
	tw.loadTablets()
	checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 0, "RemoveTablet": 1})
	checkChecksum(t, tw, 0)

	allTablets = fhc.GetAllTablets()
	key = TabletToMapKey(tablet)
	if _, ok := allTablets[key]; ok || len(allTablets) != 0 {
		t.Errorf("fhc.GetAllTablets() = %+v; don't want %v", allTablets, key)
	}
	key = TabletToMapKey(tablet2)
	if _, ok := allTablets[key]; ok || len(allTablets) != 0 {
		t.Errorf("fhc.GetAllTablets() = %+v; don't want %v", allTablets, key)
	}

	tw.Stop()
}

func TestFilterByShard(t *testing.T) {
	testcases := []struct {
		filters  []string
		keyspace string
		shard    string
		included bool
	}{
		// un-sharded keyspaces
		{
			filters:  []string{"ks1|0"},
			keyspace: "ks1",
			shard:    "0",
			included: true,
		},
		{
			filters:  []string{"ks1|0"},
			keyspace: "ks2",
			shard:    "0",
			included: false,
		},
		// custom sharding, different shard
		{
			filters:  []string{"ks1|0"},
			keyspace: "ks1",
			shard:    "1",
			included: false,
		},
		// keyrange based sharding
		{
			filters:  []string{"ks1|-80"},
			keyspace: "ks1",
			shard:    "0",
			included: false,
		},
		{
			filters:  []string{"ks1|-80"},
			keyspace: "ks1",
			shard:    "-40",
			included: true,
		},
		{
			filters:  []string{"ks1|-80"},
			keyspace: "ks1",
			shard:    "-80",
			included: true,
		},
		{
			filters:  []string{"ks1|-80"},
			keyspace: "ks1",
			shard:    "80-",
			included: false,
		},
		{
			filters:  []string{"ks1|-80"},
			keyspace: "ks1",
			shard:    "c0-",
			included: false,
		},
	}

	for _, tc := range testcases {
		fbs, err := NewFilterByShard(tc.filters)
		if err != nil {
			t.Errorf("cannot create FilterByShard for filters %v: %v", tc.filters, err)
		}

		tablet := &topodatapb.Tablet{
			Keyspace: tc.keyspace,
			Shard:    tc.shard,
		}

		got := fbs.IsIncluded(tablet)
		if got != tc.included {
			t.Errorf("isIncluded(%v,%v) for filters %v returned %v but expected %v", tc.keyspace, tc.shard, tc.filters, got, tc.included)
		}
	}
}

var (
	testFilterByKeyspace = []struct {
		keyspace string
		expected bool
	}{
		{"ks1", true},
		{"ks2", true},
		{"ks3", false},
		{"ks4", true},
		{"ks5", true},
		{"ks6", false},
		{"ks7", false},
	}
	testKeyspacesToWatch = []string{"ks1", "ks2", "ks4", "ks5"}
	testCell             = "testCell"
	testShard            = "testShard"
	testHostName         = "testHostName"
)

func TestFilterByKeyspace(t *testing.T) {
	ctx := utils.LeakCheckContext(t)

	hc := NewFakeHealthCheck(nil)
	f := NewFilterByKeyspace(testKeyspacesToWatch)
	ts := memorytopo.NewServer(ctx, testCell)
	defer ts.Close()
	tw := NewCellTabletsWatcher(context.Background(), ts, hc, f, testCell, 10*time.Minute, true, 5)

	for _, test := range testFilterByKeyspace {
		// Add a new tablet to the topology.
		port := rand.Int31n(1000)
		tablet := &topodatapb.Tablet{
			Alias: &topodatapb.TabletAlias{
				Cell: testCell,
				Uid:  rand.Uint32(),
			},
			Hostname: testHostName,
			PortMap: map[string]int32{
				"vt": port,
			},
			Keyspace: test.keyspace,
			Shard:    testShard,
		}

		got := f.IsIncluded(tablet)
		if got != test.expected {
			t.Errorf("isIncluded(%v) for keyspace %v returned %v but expected %v", test.keyspace, test.keyspace, got, test.expected)
		}

		if err := ts.CreateTablet(context.Background(), tablet); err != nil {
			t.Errorf("CreateTablet failed: %v", err)
		}

		tw.loadTablets()
		key := TabletToMapKey(tablet)
		allTablets := hc.GetAllTablets()

		if _, ok := allTablets[key]; ok != test.expected && proto.Equal(allTablets[key], tablet) != test.expected {
			t.Errorf("Error adding tablet - got %v; want %v", ok, test.expected)
		}

		// Replace the tablet we added above
		tabletReplacement := &topodatapb.Tablet{
			Alias: &topodatapb.TabletAlias{
				Cell: testCell,
				Uid:  rand.Uint32(),
			},
			Hostname: testHostName,
			PortMap: map[string]int32{
				"vt": port,
			},
			Keyspace: test.keyspace,
			Shard:    testShard,
		}
		got = f.IsIncluded(tabletReplacement)
		if got != test.expected {
			t.Errorf("isIncluded(%v) for keyspace %v returned %v but expected %v", test.keyspace, test.keyspace, got, test.expected)
		}
		if err := ts.CreateTablet(context.Background(), tabletReplacement); err != nil {
			t.Errorf("CreateTablet failed: %v", err)
		}

		tw.loadTablets()
		key = TabletToMapKey(tabletReplacement)
		allTablets = hc.GetAllTablets()

		if _, ok := allTablets[key]; ok != test.expected && proto.Equal(allTablets[key], tabletReplacement) != test.expected {
			t.Errorf("Error replacing tablet - got %v; want %v", ok, test.expected)
		}

		// Delete the tablet
		if err := ts.DeleteTablet(context.Background(), tabletReplacement.Alias); err != nil {
			t.Fatalf("DeleteTablet failed: %v", err)
		}
	}
}

// TestFilterByKeypsaceSkipsIgnoredTablets confirms a bug fix for the case when a TopologyWatcher
// has a FilterByKeyspace TabletFilter configured along with refreshKnownTablets turned off. We want
// to ensure that the TopologyWatcher:
//   - does not continuosly call GetTablets for tablets that do not satisfy the filter
//   - does not add or remove these filtered out tablets from the its healtcheck
func TestFilterByKeypsaceSkipsIgnoredTablets(t *testing.T) {
	ctx := utils.LeakCheckContext(t)

	ts := memorytopo.NewServer(ctx, "aa")
	defer ts.Close()
	fhc := NewFakeHealthCheck(nil)
	defer fhc.Close()
	topologyWatcherOperations.ZeroAll()
	counts := topologyWatcherOperations.Counts()
	f := NewFilterByKeyspace(testKeyspacesToWatch)
	tw := NewCellTabletsWatcher(context.Background(), ts, fhc, f, "aa", 10*time.Minute, false /*refreshKnownTablets*/, 5)

	counts = checkOpCounts(t, counts, map[string]int64{})
	checkChecksum(t, tw, 0)

	// Add a tablet from a tracked keyspace to the topology.
	tablet := &topodatapb.Tablet{
		Alias: &topodatapb.TabletAlias{
			Cell: "aa",
			Uid:  0,
		},
		Hostname: "host1",
		PortMap: map[string]int32{
			"vt": 123,
		},
		Keyspace: "ks1",
		Shard:    "shard",
	}
	require.NoError(t, ts.CreateTablet(context.Background(), tablet))

	tw.loadTablets()
	counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 1, "AddTablet": 1})
	checkChecksum(t, tw, 3238442862)

	// Check tablet is reported by HealthCheck
	allTablets := fhc.GetAllTablets()
	key := TabletToMapKey(tablet)
	assert.Contains(t, allTablets, key)
	assert.True(t, proto.Equal(tablet, allTablets[key]))

	// Add a second tablet to the topology that should get filtered out by the keyspace filter
	tablet2 := &topodatapb.Tablet{
		Alias: &topodatapb.TabletAlias{
			Cell: "aa",
			Uid:  2,
		},
		Hostname: "host2",
		PortMap: map[string]int32{
			"vt": 789,
		},
		Keyspace: "ks3",
		Shard:    "shard",
	}
	require.NoError(t, ts.CreateTablet(context.Background(), tablet2))

	tw.loadTablets()
	counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "GetTablet": 1})
	checkChecksum(t, tw, 2762153755)

	// Check the new tablet is NOT reported by HealthCheck.
	allTablets = fhc.GetAllTablets()
	assert.Len(t, allTablets, 1)
	key = TabletToMapKey(tablet2)
	assert.NotContains(t, allTablets, key)

	// Load the tablets again to show that when refreshKnownTablets is disabled,
	// only the list is read from the topo and the checksum doesn't change
	tw.loadTablets()
	counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1})
	checkChecksum(t, tw, 2762153755)

	// With refreshKnownTablets set to false, changes to the port map for the same tablet alias
	// should not be reflected in the HealtCheck state
	_, err := ts.UpdateTabletFields(context.Background(), tablet.Alias, func(t *topodatapb.Tablet) error {
		t.PortMap["vt"] = 456
		return nil
	})
	require.NoError(t, err)

	tw.loadTablets()
	counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1})
	checkChecksum(t, tw, 2762153755)

	allTablets = fhc.GetAllTablets()
	assert.Len(t, allTablets, 1)
	origKey := TabletToMapKey(tablet)
	tabletWithNewPort := tablet.CloneVT()
	tabletWithNewPort.PortMap["vt"] = 456
	keyWithNewPort := TabletToMapKey(tabletWithNewPort)
	assert.Contains(t, allTablets, origKey)
	assert.NotContains(t, allTablets, keyWithNewPort)

	// Remove the tracked tablet from the topo and check that it is detected as being gone.
	require.NoError(t, ts.DeleteTablet(context.Background(), tablet.Alias))

	tw.loadTablets()
	counts = checkOpCounts(t, counts, map[string]int64{"ListTablets": 1, "RemoveTablet": 1})
	checkChecksum(t, tw, 789108290)
	assert.Empty(t, fhc.GetAllTablets())

	// Remove ignored tablet and check that we didn't try to remove it from the health check
	require.NoError(t, ts.DeleteTablet(context.Background(), tablet2.Alias))

	tw.loadTablets()
	checkOpCounts(t, counts, map[string]int64{"ListTablets": 1})
	checkChecksum(t, tw, 0)
	assert.Empty(t, fhc.GetAllTablets())

	tw.Stop()
}
