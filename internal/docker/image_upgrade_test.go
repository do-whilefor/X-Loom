//go:build linux

package docker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

// Exercise an actual mutable tag upgrade without modifying any user tag or
// project container. Both source images must already exist on the local Engine.
func TestDockerImageUpgradePreservesStoppedWorkspace(t *testing.T) {
	previous, next := os.Getenv("XLOOM_DOCKER_TEST_IMAGE"), os.Getenv("XLOOM_DOCKER_UPGRADE_IMAGE")
	if previous == "" || next == "" {
		t.Skip("set XLOOM_DOCKER_TEST_IMAGE and XLOOM_DOCKER_UPGRADE_IMAGE to distinct local images")
	}
	namespace := fmt.Sprintf("xloom-image-test-%d", time.Now().UnixNano())
	ref := namespace + ":upgrade"
	c := New(config.Container{Image: ref, Network: "none", Socket: "/var/run/docker.sock", Namespace: namespace, CompletedAction: "stop"})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	imageID := func(ref string) string {
		t.Helper()
		var image struct {
			ID string `json:"Id"`
		}
		if err := c.json(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil, &image); err != nil || image.ID == "" {
			t.Fatalf("resolve test image %s: %v", ref, err)
		}
		return image.ID
	}
	oldID, nextID := imageID(previous), imageID(next)
	if oldID == nextID {
		t.Fatal("image upgrade test requires two distinct images")
	}
	setTag := func(id string) {
		t.Helper()
		if err := c.json(ctx, "POST", "/images/"+url.PathEscape(id)+"/tag?repo="+url.QueryEscape(namespace)+"&tag=upgrade", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	setTag(oldID)
	defer func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		// This removes only the unique test tag; both user's original tags stay.
		if err := c.json(cleanupCtx, "DELETE", "/images/"+url.PathEscape(ref)+"?noprune=true", nil, nil); err != nil {
			t.Error(err)
		}
	}()
	const project = "saved-workspace"
	defer func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		if err := c.Cleanup(cleanupCtx, project, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	name, err := c.ensure(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	const artifact = "/workspace/.xloom/runs/preserved/evidence.txt"
	const evidence = "immutable-image-upgrade-keeps-existing-evidence\n"
	if err := c.archive(ctx, name, artifact, []byte(evidence)); err != nil {
		t.Fatal(err)
	}
	if err := c.json(ctx, "POST", "/containers/"+name+"/stop?t=1", nil, nil); err != nil {
		t.Fatal(err)
	}
	type inspection struct {
		ID    string `json:"Id"`
		Image string
		State struct {
			Running   bool
			StartedAt string
		}
	}
	inspect := func() inspection {
		t.Helper()
		var info inspection
		if err := c.json(ctx, "GET", "/containers/"+name+"/json", nil, &info); err != nil {
			t.Fatal(err)
		}
		return info
	}
	before := inspect()
	setTag(nextID)
	_, err = c.Run(ctx, config.Worker{}, worker.Job{RunID: "must-not-exist", Kind: "reason", Graph: board.Graph{Project: board.Project{ID: project}}})
	if err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected image-upgrade rejection: %v", err)
	}
	after := inspect()
	if before != after || after.State.Running || after.Image != oldID {
		t.Fatalf("saved container was changed or restarted: before=%+v after=%+v", before, after)
	}
	response, err := c.request(ctx, "GET", "/containers/"+name+"/archive?path="+url.QueryEscape(artifact), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	archive := tar.NewReader(response.Body)
	if _, err := archive.Next(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(archive)
	if err != nil || string(data) != evidence {
		t.Fatalf("existing evidence changed: %q %v", data, err)
	}
	response, err = c.request(ctx, "GET", "/containers/"+name+"/archive?path="+url.QueryEscape("/workspace/.xloom/runs/must-not-exist"), nil, "")
	if response != nil {
		response.Body.Close()
	}
	if !status(err, 404) {
		t.Fatalf("rejected launch wrote a new task directory: %v", err)
	}
}
