// internal/global/replication_test.go
package global

import (
	"context"
	"testing"
	"time"
)

func TestNewReplicationManager(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	if rm == nil {
		t.Fatal("expected non-nil manager")
	}
	if rm.config == nil {
		t.Error("expected default config")
	}
}

func TestReplicationManagerWithConfig(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &ReplicationConfig{
		WorkerCount: 8,
		QueueSize:   500,
	}
	rm := NewReplicationManager(em, config)

	if rm.config.WorkerCount != 8 {
		t.Errorf("expected 8 workers, got %d", rm.config.WorkerCount)
	}
}

func TestReplicationManagerAddPolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	err := rm.AddPolicy(&ReplicationPolicy{
		ID:            "test-policy",
		Name:          "Test Policy",
		Enabled:       true,
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west", "eu-west"},
	})

	if err != nil {
		t.Fatalf("failed to add policy: %v", err)
	}

	policy, ok := rm.GetPolicy("test-policy")
	if !ok {
		t.Fatal("policy not found")
	}
	if policy.Name != "Test Policy" {
		t.Errorf("unexpected name: %s", policy.Name)
	}
	if policy.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}

func TestReplicationManagerAddPolicyNoID(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	err := rm.AddPolicy(&ReplicationPolicy{
		Name: "No ID",
	})

	if err == nil {
		t.Error("expected error for missing ID")
	}
}

func TestReplicationManagerRemovePolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{ID: "test-policy"})

	removed := rm.RemovePolicy("test-policy")
	if !removed {
		t.Error("expected policy to be removed")
	}

	_, ok := rm.GetPolicy("test-policy")
	if ok {
		t.Error("policy should not exist after removal")
	}

	removed = rm.RemovePolicy("nonexistent")
	if removed {
		t.Error("should not remove nonexistent policy")
	}
}

func TestReplicationManagerGetPolicies(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{ID: "policy-1"})
	_ = rm.AddPolicy(&ReplicationPolicy{ID: "policy-2"})

	policies := rm.GetPolicies()
	if len(policies) != 2 {
		t.Errorf("expected 2 policies, got %d", len(policies))
	}
}

func TestReplicationManagerEnableDisablePolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{ID: "test-policy", Enabled: true})

	err := rm.DisablePolicy("test-policy")
	if err != nil {
		t.Fatalf("failed to disable: %v", err)
	}

	policy, _ := rm.GetPolicy("test-policy")
	if policy.Enabled {
		t.Error("expected policy to be disabled")
	}

	err = rm.EnablePolicy("test-policy")
	if err != nil {
		t.Fatalf("failed to enable: %v", err)
	}

	policy, _ = rm.GetPolicy("test-policy")
	if !policy.Enabled {
		t.Error("expected policy to be enabled")
	}
}

func TestReplicationManagerEnableNonexistent(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	err := rm.EnablePolicy("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent policy")
	}

	err = rm.DisablePolicy("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent policy")
	}
}

func TestReplicationManagerSubmitTask(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	// Initialize region status
	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "init",
		TargetRegions: []string{"us-west"},
	})

	task := &ReplicationTask{
		SourceRegion: "us-east",
		TargetRegion: "us-west",
		Container:    "bucket",
		Key:          "file.txt",
		Size:         1024,
	}

	err := rm.SubmitTask(task)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	if task.ID == "" {
		t.Error("expected task ID to be set")
	}
	if task.State != TaskStatePending {
		t.Errorf("expected pending state, got %s", task.State)
	}

	retrieved, ok := rm.GetTask(task.ID)
	if !ok {
		t.Fatal("task not found")
	}
	if retrieved.Container != "bucket" {
		t.Error("task not saved correctly")
	}
}

func TestReplicationManagerGetPendingTasks(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "init",
		TargetRegions: []string{"us-west", "eu-west"},
	})

	_ = rm.SubmitTask(&ReplicationTask{TargetRegion: "us-west"})
	_ = rm.SubmitTask(&ReplicationTask{TargetRegion: "us-west"})
	_ = rm.SubmitTask(&ReplicationTask{TargetRegion: "eu-west"})

	usWestTasks := rm.GetPendingTasks("us-west")
	if len(usWestTasks) != 2 {
		t.Errorf("expected 2 us-west tasks, got %d", len(usWestTasks))
	}

	euWestTasks := rm.GetPendingTasks("eu-west")
	if len(euWestTasks) != 1 {
		t.Errorf("expected 1 eu-west task, got %d", len(euWestTasks))
	}
}

func TestReplicationManagerRegionStatus(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "test",
		TargetRegions: []string{"us-west"},
	})

	status, ok := rm.GetRegionStatus("us-west")
	if !ok {
		t.Fatal("expected region status")
	}
	if status.State != ReplicaStateActive {
		t.Errorf("expected active state, got %s", status.State)
	}

	rm.SetRegionState("us-west", ReplicaStateLagging)
	status, _ = rm.GetRegionStatus("us-west")
	if status.State != ReplicaStateLagging {
		t.Error("state not updated")
	}
}

func TestReplicationManagerGetAllRegionStatus(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "test",
		TargetRegions: []string{"us-west", "eu-west"},
	})

	allStatus := rm.GetAllRegionStatus()
	if len(allStatus) != 2 {
		t.Errorf("expected 2 regions, got %d", len(allStatus))
	}
}

func TestReplicationManagerStartStop(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	rm.Start()
	time.Sleep(10 * time.Millisecond)
	rm.Stop()
	// Should not hang or panic
}

func TestReplicationManagerProcessTask(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &ReplicationConfig{
		WorkerCount: 1,
		QueueSize:   10,
	}
	rm := NewReplicationManager(em, config)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "init",
		TargetRegions: []string{"us-west"},
	})

	rm.Start()
	defer rm.Stop()

	task := &ReplicationTask{
		SourceRegion: "us-east",
		TargetRegion: "us-west",
		Container:    "bucket",
		Key:          "file.txt",
		Size:         1024,
	}

	_ = rm.SubmitTask(task)

	// Wait for processing
	time.Sleep(100 * time.Millisecond)

	retrieved, _ := rm.GetTask(task.ID)
	if retrieved.State != TaskStateCompleted {
		t.Errorf("expected completed state, got %s", retrieved.State)
	}
}

func TestReplicationManagerMetrics(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "init",
		TargetRegions: []string{"us-west"},
	})

	_ = rm.SubmitTask(&ReplicationTask{TargetRegion: "us-west", Size: 1024})
	_ = rm.SubmitTask(&ReplicationTask{TargetRegion: "us-west", Size: 2048})

	metrics := rm.GetMetrics()
	if metrics.TotalTasks != 2 {
		t.Errorf("expected 2 total tasks, got %d", metrics.TotalTasks)
	}
}

func TestReplicationManagerReplicateObject(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "us-multi",
		Enabled:       true,
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west", "eu-west"},
	})

	ctx := context.Background()
	tasks, err := rm.ReplicateObject(ctx, "us-east", "bucket", "file.txt", 1024)

	if err != nil {
		t.Fatalf("failed to replicate: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestReplicationManagerReplicateObjectNoPolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	ctx := context.Background()
	tasks, err := rm.ReplicateObject(ctx, "us-east", "bucket", "file.txt", 1024)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestReplicationManagerReplicateObjectDisabledPolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "disabled",
		Enabled:       false,
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
	})

	ctx := context.Background()
	tasks, _ := rm.ReplicateObject(ctx, "us-east", "bucket", "file.txt", 1024)

	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for disabled policy, got %d", len(tasks))
	}
}

func TestReplicationManagerFilters(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "filtered",
		Enabled:       true,
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
		Filters: []ReplicationFilter{
			{Type: FilterPrefix, Pattern: "important/", Include: true},
		},
	})

	ctx := context.Background()

	// Should match prefix
	tasks, _ := rm.ReplicateObject(ctx, "us-east", "important", "file.txt", 1024)
	if len(tasks) != 1 {
		t.Errorf("expected 1 task for matching prefix, got %d", len(tasks))
	}

	// Should not match prefix
	tasks, _ = rm.ReplicateObject(ctx, "us-east", "other", "file.txt", 1024)
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for non-matching prefix, got %d", len(tasks))
	}
}

func TestReplicationManagerWaitForReplication(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &ReplicationConfig{
		WorkerCount: 2,
		QueueSize:   10,
	}
	rm := NewReplicationManager(em, config)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "init",
		TargetRegions: []string{"us-west"},
	})

	rm.Start()
	defer rm.Stop()

	task := &ReplicationTask{
		TargetRegion: "us-west",
		Size:         1024,
	}
	_ = rm.SubmitTask(task)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := rm.WaitForReplication(ctx, []string{task.ID})
	if err != nil {
		t.Errorf("wait failed: %v", err)
	}
}

func TestReplicationManagerWaitTimeout(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)
	// Don't start workers

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "init",
		TargetRegions: []string{"us-west"},
	})

	task := &ReplicationTask{
		TargetRegion: "us-west",
	}
	_ = rm.SubmitTask(task)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := rm.WaitForReplication(ctx, []string{task.ID})
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestReplicationManagerGenerateReport(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "active-policy",
		Enabled:       true,
		TargetRegions: []string{"us-west"},
	})
	_ = rm.AddPolicy(&ReplicationPolicy{
		ID:            "inactive-policy",
		Enabled:       false,
		TargetRegions: []string{"eu-west"},
	})

	report := rm.GenerateReport()

	if report.TotalPolicies != 2 {
		t.Errorf("expected 2 policies, got %d", report.TotalPolicies)
	}
	if report.ActivePolicies != 1 {
		t.Errorf("expected 1 active policy, got %d", report.ActivePolicies)
	}
	if report.GeneratedAt.IsZero() {
		t.Error("expected GeneratedAt to be set")
	}
}

func TestDefaultRetryPolicy(t *testing.T) {
	policy := DefaultRetryPolicy()

	if policy.MaxRetries != 5 {
		t.Errorf("expected 5 retries, got %d", policy.MaxRetries)
	}
	if policy.InitialBackoff != time.Second {
		t.Error("unexpected initial backoff")
	}
	if policy.BackoffFactor != 2.0 {
		t.Error("unexpected backoff factor")
	}
}

func TestDefaultReplicationConfig(t *testing.T) {
	config := DefaultReplicationConfig()

	if config.WorkerCount != 4 {
		t.Errorf("expected 4 workers, got %d", config.WorkerCount)
	}
	if config.QueueSize != 1000 {
		t.Errorf("expected queue size 1000, got %d", config.QueueSize)
	}
	if config.DefaultMode != ReplicationAsync {
		t.Error("expected async mode by default")
	}
}

func TestCommonReplicationPolicies(t *testing.T) {
	policies := CommonReplicationPolicies()

	if len(policies) == 0 {
		t.Fatal("expected common policies")
	}

	foundUSMulti := false
	for _, p := range policies {
		if p.ID == "us-multi-region" {
			foundUSMulti = true
			if p.SourceRegion != "us-east" {
				t.Error("expected us-east source")
			}
		}
	}
	if !foundUSMulti {
		t.Error("expected us-multi-region policy")
	}
}

func TestReplicationModes(t *testing.T) {
	modes := []ReplicationMode{
		ReplicationSync,
		ReplicationAsync,
		ReplicationQuorum,
	}

	for _, m := range modes {
		if m == "" {
			t.Error("mode should not be empty")
		}
	}
}

func TestReplicationStates(t *testing.T) {
	states := []ReplicationState{
		ReplicaStateActive,
		ReplicaStateLagging,
		ReplicaStateCatchingUp,
		ReplicaStateFailed,
		ReplicaStatePaused,
	}

	for _, s := range states {
		if s == "" {
			t.Error("state should not be empty")
		}
	}
}

func TestTaskStates(t *testing.T) {
	states := []ReplicationTaskState{
		TaskStatePending,
		TaskStateRunning,
		TaskStateCompleted,
		TaskStateFailed,
		TaskStateCancelled,
	}

	for _, s := range states {
		if s == "" {
			t.Error("state should not be empty")
		}
	}
}

func TestSelectionStrategies(t *testing.T) {
	strategies := []SelectionStrategy{
		StrategyNearest,
		StrategyRoundRobin,
		StrategyLeastLoad,
		StrategyRandom,
		StrategyManual,
	}

	for _, s := range strategies {
		if s == "" {
			t.Error("strategy should not be empty")
		}
	}
}

func TestFilterTypes(t *testing.T) {
	types := []FilterType{
		FilterPrefix,
		FilterSuffix,
		FilterRegex,
		FilterSizeMin,
		FilterSizeMax,
		FilterTag,
	}

	for _, ft := range types {
		if ft == "" {
			t.Error("filter type should not be empty")
		}
	}
}

func TestMatchesFiltersSuffix(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	filters := []ReplicationFilter{
		{Type: FilterSuffix, Pattern: ".jpg", Include: true},
	}

	// Should match
	if !rm.matchesFilters(filters, "photos", "image.jpg", 1024) {
		t.Error("expected .jpg to match")
	}

	// Should not match
	if rm.matchesFilters(filters, "photos", "image.png", 1024) {
		t.Error("expected .png not to match")
	}
}

func TestMatchesFiltersExclude(t *testing.T) {
	em := NewEdgeManager(nil)
	rm := NewReplicationManager(em, nil)

	filters := []ReplicationFilter{
		{Type: FilterPrefix, Pattern: "temp/", Include: false},
	}

	// Should match (not in temp/)
	if !rm.matchesFilters(filters, "data", "file.txt", 1024) {
		t.Error("expected non-temp to match")
	}

	// Should not match (in temp/)
	if rm.matchesFilters(filters, "temp", "file.txt", 1024) {
		t.Error("expected temp/ to not match")
	}
}
