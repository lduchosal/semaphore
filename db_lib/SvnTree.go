package db_lib

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/semaphoreui/semaphore/pkg/svn"
	"github.com/semaphoreui/semaphore/util"
)

// The working copy of a Subversion branch is shared by every template of the
// repository using the branch (Repository.GetSvnCachePath): one download and
// one copy on disk, however many templates there are. Tasks never run in it.
// Before each task, the template gets its own tree, rebuilt from the working
// copy with hard links:
//
//   - svn update replaces a file with a new one instead of writing into it, so
//     the tree of a running task keeps the files of its revision while another
//     template updates the working copy;
//   - the tree is rebuilt for every task, so files a task leaves behind never
//     reach another template, nor the next task of its own;
//   - the files of the working copy are read-only, so a task cannot write
//     through a link into the files of other templates; tools which rewrite a
//     file (ansible template, sed -i) replace it in the tree instead.
//
// All tasks run as the same system user, so this guards the integrity of what
// a task runs against overlapping tasks, not against a task bent on tampering.

// svnCacheDir runs svn in the shared working copy of the repository branch.
const svnCacheDir GitRepositoryDirType = -1

// svnTreeInfo records what the tree of a template was built from. It is
// written beside the tree because the shared working copy may have moved on
// by the time the task asks for its commit.
type svnTreeInfo struct {
	Revision string `json:"revision"`
	Message  string `json:"message"`
}

func svnTreeInfoPath(r GitRepository) string {
	return r.GetFullPath() + ".svn.json"
}

func (c SvnClient) Clone(r GitRepository) error {
	return c.sync(r, "")
}

func (c SvnClient) Pull(r GitRepository) error {
	return c.sync(r, "")
}

func (c SvnClient) Checkout(r GitRepository, target string) error {
	if target == "" {
		return fmt.Errorf("task commit hash is empty")
	}
	if err := svn.ValidateRevision(target, "task"); err != nil {
		return err
	}

	return c.sync(r, target)
}

// CanBePulled is always true: sync brings the shared working copy and the
// tree of the template up to date whatever state they are in.
func (c SvnClient) CanBePulled(r GitRepository) bool {
	return true
}

func (c SvnClient) GetLastCommitHash(r GitRepository) (string, error) {
	info, err := readSvnTreeInfo(r)
	return info.Revision, err
}

func (c SvnClient) GetLastCommitMessage(r GitRepository) (string, error) {
	info, err := readSvnTreeInfo(r)
	return info.Message, err
}

// sync updates the shared working copy to the revision (HEAD when empty) and
// rebuilds the tree of the template from it, holding the lock of the working
// copy so that no other template updates it in between.
func (c SvnClient) sync(r GitRepository, revision string) error {
	branchURL, err := svnBranchURL(r)
	if err != nil {
		return err
	}

	cache := r.Repository.GetSvnCachePath()
	if r.Lock != nil {
		unlock := r.Lock(cache)
		defer unlock()
	}

	if err = c.updateCache(r, branchURL, revision); err != nil {
		return err
	}

	info, err := c.cacheInfo(r)
	if err != nil {
		return err
	}

	if err = makeReadOnly(cache); err != nil {
		return err
	}

	return replaceTree(cache, r.GetFullPath(), info)
}

func (c SvnClient) updateCache(r GitRepository, branchURL string, revision string) error {
	wcURL, err := c.output(r, svnCacheDir, "info", "--show-item", "url")
	if err == nil && svn.SameURL(wcURL, branchURL) {
		r.Logger.Log("Updating Subversion repository " + r.Repository.GetRedactedGitURL())

		err = c.revertAndUpdate(r, revision)
		if err == nil || !svnNeedsCleanup(err) {
			return err
		}

		// An interrupted checkout or update leaves the working copy locked.
		// Cleaning up keeps what was already downloaded.
		r.Logger.Log("Cleaning up the interrupted Subversion working copy")
		if err = c.run(r, svnCacheDir, "cleanup"); err == nil {
			if err = c.revertAndUpdate(r, revision); err == nil {
				return nil
			}
		}
		r.Logger.Log("Cleaning up failed (" + err.Error() + "), checking out again")
	}

	// No working copy yet, one of another URL (repository edits clear the
	// cache of this server only, a runner may still hold the old one), or one
	// cleanup could not repair.
	r.Logger.Log("Checking out Subversion repository " + r.Repository.GetRedactedGitURL())

	cache := r.Repository.GetSvnCachePath()
	if err = os.RemoveAll(cache); err != nil {
		return err
	}
	if err = os.MkdirAll(cache, 0755); err != nil {
		return err
	}
	if err = util.ChownDir(cache); err != nil {
		return err
	}

	args := []string{"checkout"}
	if revision != "" {
		args = append(args, "--revision", revision)
	}
	args = append(args, "--", svnPeg(branchURL), cache)

	return c.run(r, GitRepositoryTmpPath, args...)
}

// revertAndUpdate reverts local modifications first: svn update merges into
// them and reports a conflict as success. Tasks do not run in the working copy,
// so there should be none; this keeps a stray one from reaching every template.
func (c SvnClient) revertAndUpdate(r GitRepository, revision string) error {
	if err := c.run(r, svnCacheDir, "revert", "--recursive", "."); err != nil {
		return err
	}

	args := []string{"update"}
	if revision != "" {
		args = append(args, "--revision", revision)
	}
	return c.run(r, svnCacheDir, args...)
}

// svnNeedsCleanup reports whether svn refused to work on a working copy left
// locked by an interrupted command (E155004 working copy locked, E155037
// previous operation not finished).
func svnNeedsCleanup(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "E155004") || strings.Contains(msg, "E155037")
}

type svnLog struct {
	Entries []struct {
		Revision string `xml:"revision,attr"`
		Msg      string `xml:"msg"`
	} `xml:"logentry"`
}

// cacheInfo returns the last revision which changed the branch path, not the
// revision of the whole repository, so a commit elsewhere in the repository
// does not look like a change to this one, and the subject of its message.
func (c SvnClient) cacheInfo(r GitRepository) (info svnTreeInfo, err error) {
	info.Revision, err = c.output(r, svnCacheDir, "info", "--show-item", "last-changed-revision")
	if err != nil {
		return
	}

	out, err := c.output(r, svnCacheDir, "log", "--xml", "--limit", "1", "--revision", info.Revision)
	if err != nil {
		return
	}

	var entries svnLog
	if err = xml.Unmarshal([]byte(out), &entries); err != nil {
		return
	}

	if len(entries.Entries) > 0 {
		// show-branch prints the subject line only; do the same.
		msg, _, _ := strings.Cut(strings.TrimSpace(entries.Entries[0].Msg), "\n")
		info.Message = truncateCommitMessage(msg)
	}

	return
}

// makeReadOnly removes the write permission of the files of the working copy,
// which the trees of the templates link to. svn replaces a file to update it,
// which needs the directory to be writable only.
func makeReadOnly(cache string) error {
	return filepath.WalkDir(cache, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".svn" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}
		if mode := fi.Mode().Perm(); mode&0222 != 0 {
			return os.Chmod(p, mode&^0222)
		}
		return nil
	})
}

// replaceTree builds the tree of a template from the working copy beside the
// current one, then swaps them. The previous tree is kept as <tree>.old until
// the next swap, for a task of the same template still running from it.
func replaceTree(cache string, tree string, info svnTreeInfo) error {
	parent, base := filepath.Dir(tree), filepath.Base(tree)

	// Trees left half-built by an interrupted swap. The template is locked by
	// the caller, so none of them is being built.
	if leftovers, err := filepath.Glob(filepath.Join(parent, base+".new-*")); err == nil {
		for _, p := range leftovers {
			_ = os.RemoveAll(p)
		}
	}

	next, err := os.MkdirTemp(parent, base+".new-")
	if err != nil {
		return err
	}

	if err = linkTree(cache, next); err != nil {
		_ = os.RemoveAll(next)
		return err
	}

	old := tree + ".old"
	if err = os.RemoveAll(old); err != nil {
		return err
	}
	if err = os.Rename(tree, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err = os.Rename(next, tree); err != nil {
		return err
	}

	return writeSvnTreeInfo(tree+".svn.json", info)
}

// linkTree recreates the working copy at dst without its .svn directories,
// linking its files. A file which cannot be linked is copied.
func linkTree(src string, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case d.IsDir():
			if d.Name() == ".svn" {
				return filepath.SkipDir
			}
			if err = os.MkdirAll(target, 0755); err != nil {
				return err
			}
			if err = os.Chmod(target, 0755); err != nil {
				return err
			}
			return util.ChownDir(target)

		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)

		case d.Type().IsRegular():
			if err = os.Link(p, target); err == nil {
				return nil
			}
			return copyFile(p, target)

		default:
			return nil
		}
	})
}

func copyFile(src string, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writeSvnTreeInfo(p string, info svnTreeInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}

	tmp := p + ".tmp"
	if err = os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func readSvnTreeInfo(r GitRepository) (info svnTreeInfo, err error) {
	data, err := os.ReadFile(svnTreeInfoPath(r))
	if err != nil {
		return
	}
	err = json.Unmarshal(data, &info)
	return
}
