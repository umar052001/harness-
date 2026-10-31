package api

import "testing"

func TestServiceChangesAndJobsCannotOverlap(t *testing.T) {
	r := newRunRegistry(1)
	if !r.beginMaintenance() {
		t.Fatal("idle service operation rejected")
	}
	if _, err := r.acquire(ActiveRun{}); err == nil {
		t.Fatal("job started during service change")
	}
	if r.beginMaintenance() {
		t.Fatal("concurrent service operation accepted")
	}
	r.endMaintenance()
	token, err := r.acquire(ActiveRun{})
	if err != nil {
		t.Fatal(err)
	}
	if r.beginMaintenance() {
		t.Fatal("database could be stopped during a job")
	}
	r.release(token)
	if !r.beginMaintenance() {
		t.Fatal("service guard was not released")
	}
	r.endMaintenance()
}
