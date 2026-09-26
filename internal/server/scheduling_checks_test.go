package server

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
)

func TestSchedulingChecksAreOptionalAndPaged(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		g, err := tx.Load(f.project)
		if err != nil {
			return err
		}
		for n := 0; n < 105; n++ {
			g.Intents = append(g.Intents, board.Intent{ID: fmt.Sprintf("s%03d", n), From: []string{"origin"}, Description: "Observe fixture", Creator: "fixture", CreatedAt: tx.Now})
		}
		return tx.Save(g)
	}); err != nil {
		t.Fatal(err)
	}
	var legacy, first, next board.SchedulePage
	f.request("GET", f.base()+"/scheduling", nil, false, http.StatusOK, &legacy)
	if legacy.ExecutionChecks != nil {
		t.Fatal("legacy caller unexpectedly requested registry checks")
	}
	f.request("GET", f.base()+"/scheduling?namespace=test", nil, false, http.StatusOK, &first)
	if len(first.Intents) != 100 || len(first.ExecutionChecks) != 100 || first.NextOffset != 100 {
		t.Fatalf("first page: intents=%d checks=%d next=%d", len(first.Intents), len(first.ExecutionChecks), first.NextOffset)
	}
	f.request("GET", f.base()+"/scheduling?namespace=test&offset=100&expected_version="+first.StateVersion, nil, false, http.StatusOK, &next)
	if len(next.Intents) != 5 || len(next.ExecutionChecks) != 5 || next.NextOffset != 0 {
		t.Fatalf("next page: intents=%d checks=%d next=%d", len(next.Intents), len(next.ExecutionChecks), next.NextOffset)
	}
	for _, page := range []board.SchedulePage{first, next} {
		for _, intent := range page.Intents {
			check, ok := page.ExecutionChecks["explore:"+intent.ID]
			if !ok || check.Blocked || check.Pending {
				t.Fatalf("missing or blocked new candidate %s: %+v", intent.ID, check)
			}
		}
	}
	var all board.SchedulePage
	f.request("GET", f.base()+"/scheduling?namespace=test&limit=1000", nil, false, http.StatusOK, &all)
	combined := first
	combined.Intents = append(combined.Intents, next.Intents...)
	combined.Steps = append(combined.Steps, next.Steps...)
	combined.NextOffset = 0
	for key, check := range next.ExecutionChecks {
		combined.ExecutionChecks[key] = check
	}
	if len(all.ExecutionChecks) != 105 || !reflect.DeepEqual(all, combined) {
		t.Fatal("full compact schedule lost page metadata or execution admission checks")
	}
	var withoutChecks board.SchedulePage
	f.request("GET", f.base()+"/scheduling?limit=1000", nil, false, http.StatusOK, &withoutChecks)
	if len(withoutChecks.Intents) != 105 || withoutChecks.ExecutionChecks != nil {
		t.Fatal("full compact schedule changed optional registry checks")
	}
	for _, query := range []string{"limit=invalid", "limit=", "limit=0", "limit=-1", "limit=1001"} {
		f.request("GET", f.base()+"/scheduling?"+query, nil, false, http.StatusUnprocessableEntity, nil)
	}
	for _, namespace := range []string{"", strings.Repeat("n", 129)} {
		f.request("GET", f.base()+"/scheduling?namespace="+namespace, nil, false, http.StatusUnprocessableEntity, nil)
	}
	f.request("GET", f.base()+"/scheduling?namespace=test&expected_version=outdated", nil, false, http.StatusConflict, nil)
	f.request("GET", f.base()+"/scheduling?namespace=test&limit=1000&expected_version=outdated", nil, false, http.StatusConflict, nil)
}

func TestSchedulingLargePageKeepsAdmissionAfterFirstHundred(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		g, err := tx.Load(f.project)
		if err != nil {
			return err
		}
		for n := 0; n < 105; n++ {
			g.Intents = append(g.Intents, board.Intent{ID: fmt.Sprintf("s%03d", n), From: []string{"origin"}, Description: "Observe fixture", Creator: "fixture", CreatedAt: tx.Now})
		}
		g.Intents[104].Worker = board.Ptr("claimed@run")
		if err := tx.Save(g); err != nil {
			return err
		}
		for n, status := range []string{"running", "failed", "retry_requested"} {
			id := fmt.Sprintf("s%03d", 101+n)
			_, err := tx.Exec(`INSERT INTO xloom_executions(project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,created_at,updated_at) VALUES(?,?,'test','worker','explore',?,?,'{}',?,?,?,?)`, f.project, "run-"+id, id, "worker@run-"+id, "explore:"+id, status, tx.Now, tx.Now)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var all board.SchedulePage
	f.request("GET", f.base()+"/scheduling?namespace=test&limit=1000", nil, false, http.StatusOK, &all)
	if len(all.Intents) != 105 || len(all.ExecutionChecks) != 104 || !all.ExecutionChecks["explore:s101"].Pending || !all.ExecutionChecks["explore:s102"].Blocked || all.ExecutionChecks["explore:s103"].Blocked || all.ExecutionChecks["explore:s103"].PreviousRunID != "run-s103" {
		t.Fatal("full scheduling lost pending, failed or human retry admission after the first chunk")
	}
	if _, present := all.ExecutionChecks["explore:s104"]; present || board.Value(all.Intents[104].Worker) != "claimed@run" {
		t.Fatal("full scheduling made a claimed step available")
	}
}
