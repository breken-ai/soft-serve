package backend

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/matryer/is"
)

// fakeLFSClient serves object contents from memory and records every
// download request it receives.
type fakeLFSClient struct {
	objects   map[string]string
	requested [][]lfs.Pointer
}

func (c *fakeLFSClient) Download(_ context.Context, objects []lfs.Pointer, callback lfs.DownloadCallback) error {
	c.requested = append(c.requested, objects)
	for _, p := range objects {
		if err := callback(p, io.NopCloser(strings.NewReader(c.objects[p.Oid])), nil); err != nil {
			return err
		}
	}
	return nil
}

func (*fakeLFSClient) Upload(context.Context, []lfs.Pointer, lfs.UploadCallback) error {
	return nil
}

// TestStoreRepoMissingLFSObjects verifies that LFS objects referenced by a
// repository are downloaded even when there are fewer than a full batch of
// them, and that a later sync does not download them again.
func TestStoreRepoMissingLFSObjects(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := config.WithContext(context.Background(), cfg)

	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	repo, err := be.CreateRepository(ctx, "lfs-repo", admin, proto.RepositoryOptions{})
	is.NoErr(err)

	// Push one commit with a full batch of LFS pointers plus one more, so
	// the sync has to handle both a full and a partial batch.
	const count = 21
	work := t.TempDir()
	objects := make(map[string]string, count)
	pointers := make([]lfs.Pointer, 0, count)
	for i := range count {
		content := "lfs object " + strconv.Itoa(i)
		pointer, err := lfs.GeneratePointer(strings.NewReader(content))
		is.NoErr(err)
		objects[pointer.Oid] = content
		pointers = append(pointers, pointer)
		name := filepath.Join(work, "file"+strconv.Itoa(i)+".bin")
		is.NoErr(os.WriteFile(name, []byte(pointer.String()), 0o600))
	}

	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "."},
		{"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "add lfs pointers"},
		{"push", "-q", be.repoPath("lfs-repo"), "HEAD:refs/heads/main"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = work
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	client := &fakeLFSClient{objects: objects}
	is.NoErr(StoreRepoMissingLFSObjects(ctx, repo, be.db, be.store, client))

	// Every object is downloaded and stored.
	var requested int
	for _, batch := range client.requested {
		requested += len(batch)
	}
	is.Equal(requested, count)

	strg := storage.NewLocalStorage(filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10)))
	for _, pointer := range pointers {
		obj, err := be.store.GetLFSObjectByOid(ctx, be.db, repo.ID(), pointer.Oid)
		is.NoErr(err)
		is.Equal(obj.Size, pointer.Size)

		exist, err := strg.Exists(path.Join("objects", pointer.RelativePath()))
		is.NoErr(err)
		is.True(exist)
	}

	// A second sync (e.g. the next mirror pull) has nothing left to fetch.
	batches := len(client.requested)
	is.NoErr(StoreRepoMissingLFSObjects(ctx, repo, be.db, be.store, client))
	is.Equal(len(client.requested), batches)
}
