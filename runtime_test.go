package main

import (
	"sync"
	"testing"
	"time"
)

func TestExecuteBackgroundCommandLifecycle(t *testing.T) {
	job, err := executeBackgroundCommand("sess1", "run1", "echo hello")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	if job.RunID != "run1" || job.SessionID != "sess1" {
		t.Fatalf("expected job to carry sessionID/runID assigned at creation, got %#v", job)
	}
	select {
	case <-job.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not complete in time")
	}
	got, ok := jobStore.get(job.ID)
	if !ok {
		t.Fatal("expected job to be present in the store")
	}
	if got.Status != "completed" {
		t.Fatalf("expected status completed, got %q (error=%q)", got.Status, got.Error)
	}
	if got.RunID != "run1" {
		t.Fatalf("expected RunID to persist across completion, got %q", got.RunID)
	}
}

func TestExecuteBackgroundCommandCancelDoesNotGetOverwrittenAsFailed(t *testing.T) {
	job, err := executeBackgroundCommand("sess2", "run2", "sleep 5")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	if !jobStore.cancel(job.ID, "sess2") {
		t.Fatal("expected cancel to find the job")
	}
	select {
	case <-job.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled job did not finish in time")
	}
	got, ok := jobStore.get(job.ID)
	if !ok {
		t.Fatal("expected job to remain in the store after cancellation")
	}
	if got.Status != "cancelled" {
		t.Fatalf("expected killed process's natural exit to not overwrite the cancelled status, got %q", got.Status)
	}
}

func TestJobStoreCancelWrongSessionIsNoop(t *testing.T) {
	job, err := executeBackgroundCommand("owner", "run3", "sleep 5")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	t.Cleanup(func() {
		jobStore.cancel(job.ID, "owner")
		<-job.Done
	})
	if jobStore.cancel(job.ID, "someone-else") {
		t.Fatal("expected cancel to reject a session that does not own the job")
	}
	got, _ := jobStore.get(job.ID)
	if got.Status != "running" {
		t.Fatalf("expected job to remain running after a rejected cancel, got %q", got.Status)
	}
}

func TestJobStoreListBySessionFiltersBySession(t *testing.T) {
	jobA, err := executeBackgroundCommand("list-sess-a", "run-a", "echo a")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	jobB, err := executeBackgroundCommand("list-sess-b", "run-b", "echo b")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	<-jobA.Done
	<-jobB.Done

	jobs := jobStore.listBySession("list-sess-a")
	if len(jobs) != 1 || jobs[0].ID != jobA.ID {
		t.Fatalf("expected only session a's job, got %#v", jobs)
	}
}

func TestJobStoreConcurrentAccessIsRaceFree(t *testing.T) {
	job, err := executeBackgroundCommand("race-sess", "race-run", "sleep 1")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = jobStore.get(job.ID)
			_ = jobStore.listBySession("race-sess")
		}()
	}
	jobStore.cancel(job.ID, "race-sess")
	wg.Wait()
	<-job.Done
}

func TestClonePlanStepsIsDeepCopy(t *testing.T) {
	original := []AgentPlanStep{{ID: 1, Status: "pending", EvidenceEventIDs: []int{1, 2}}}
	cloned := clonePlanSteps(original)
	cloned[0].Status = "completed"
	cloned[0].EvidenceEventIDs[0] = 99
	if original[0].Status != "pending" {
		t.Fatalf("mutating the clone's status must not affect the original, got %q", original[0].Status)
	}
	if original[0].EvidenceEventIDs[0] != 1 {
		t.Fatalf("mutating the clone's evidence IDs must not affect the original, got %v", original[0].EvidenceEventIDs)
	}
}

func TestAgentSnapshotPlanIsIsolatedFromConcurrentMutation(t *testing.T) {
	sess := &agentSession{
		ID: "snap-sess",
		AgentRun: &AgentRun{
			ID:   "snap-run",
			Plan: []AgentPlanStep{{ID: 1, Status: "pending", EvidenceEventIDs: []int{1}}},
		},
	}
	snapshot := sess.agentSnapshot()

	sess.mu.Lock()
	sess.AgentRun.Plan[0].Status = "completed"
	sess.AgentRun.Plan[0].EvidenceEventIDs[0] = 42
	sess.mu.Unlock()

	if snapshot.Plan[0].Status != "pending" {
		t.Fatalf("snapshot must not observe a later mutation of the live plan, got %q", snapshot.Plan[0].Status)
	}
	if snapshot.Plan[0].EvidenceEventIDs[0] != 1 {
		t.Fatalf("snapshot's evidence IDs must not share the live plan's backing array, got %v", snapshot.Plan[0].EvidenceEventIDs)
	}
}
