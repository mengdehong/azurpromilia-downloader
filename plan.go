package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type plan struct {
	PathChanges   []pathTransition `json:"path_transitions,omitempty"`
	update        *pathUpdate
	UnpackPending bool       `json:"unpack_space_pending,omitempty"`
	Build         string     `json:"build"`
	TotalFiles    int        `json:"total_files"`
	ReuseFiles    int        `json:"reuse_files"`
	TotalBytes    int64      `json:"total_bytes"`
	ReuseBytes    int64      `json:"reuse_bytes"`
	ResumeBytes   int64      `json:"resume_bytes"`
	DownloadBytes int64      `json:"download_bytes"`
	RequiredBytes int64      `json:"required_bytes"`
	Jobs          []planFile `json:"files"`
}

type planFile struct {
	Local  string   `json:"-"`
	Cached int64    `json:"resume_bytes,omitempty"`
	File   resource `json:"-"`
	Path   string   `json:"path"`
	Status string   `json:"status"`
	Bytes  int64    `json:"download_bytes"`

	checkedStage os.FileInfo
	stageValid   bool
}

func planDownload(ctx context.Context, root *os.Root, list manifest, options downloadOptions, progress io.Writer) (plan, error) {
	p := plan{Build: list.Build, TotalFiles: len(list.Files), Jobs: []planFile{}}
	var err error
	p.update, err = planPathUpdate(ctx, root, list, options.previous)
	if err != nil {
		return p, err
	}
	p.PathChanges = p.update.list()
	checks := make([]fileCheck, len(list.Files))
	if err := checkInParallel(ctx, len(checks), options.verifyWorkers, progress, func(ctx context.Context, i int) error {
		result, err := checkPlanFile(ctx, root, list.Files[i], p.update, options)
		checks[i] = result
		return err
	}); err != nil {
		return p, err
	}
	var assembly []int64
	for i, f := range list.Files {
		result := checks[i]
		p.TotalBytes += f.Size
		if result.reused {
			p.ReuseFiles++
			p.ReuseBytes += f.Size
			continue
		}
		if result.cache.assemble {
			assembly = append(assembly, f.Size)
		}
		p.ResumeBytes += result.cache.bytes
		remaining := f.Size - result.cache.bytes
		p.RequiredBytes += remaining
		downloadBytes := remaining
		if result.local != "" {
			downloadBytes = 0
		}
		p.DownloadBytes += downloadBytes
		p.Jobs = append(p.Jobs, planFile{
			File: f, Path: f.Path, Status: result.status, Local: result.local,
			Bytes: downloadBytes, Cached: result.cache.bytes,
			checkedStage: result.cache.checked, stageValid: result.cache.valid,
		})
	}
	sort.Slice(assembly, func(i, j int) bool { return assembly[i] > assembly[j] })
	for _, capacity := range assembly[:min(options.workers, len(assembly))] {
		if capacity > math.MaxInt64-p.RequiredBytes {
			p.RequiredBytes = math.MaxInt64
			break
		}
		p.RequiredBytes += capacity
	}

	return p, nil
}

type fileCheck struct {
	status, local string
	cache         stagedCache
	reused        bool
}

func checkPlanFile(ctx context.Context, root *os.Root, f resource, update *pathUpdate, opts downloadOptions) (fileCheck, error) {
	result := fileCheck{status: "missing"}
	if root == nil {
		result.cache.assemble = len(splitFile(f, opts)) > 1
		return result, nil
	}
	if update.at(f.Path) != "" {
		result.status = "layout_change"
	} else {
		size, err := regularSize(root, f.Path)
		if err != nil {
			return result, err
		}
		if size >= 0 {
			result.status = "changed"
		}
		if size == f.Size {
			valid, err := verifyDigest(ctx, root, f.Path, f)
			if valid || err != nil {
				result.reused = valid
				return result, err
			}
		}
	}
	if opts.parts > 0 {
		var err error
		result.cache, err = checkCache(ctx, root, f, opts)
		if err != nil {
			return result, err
		}
	}
	if update == nil {
		return result, nil
	}
	old := update.oldFiles[strings.ToLower(f.Path)]
	if old.Path == "" || old.Path == f.Path {
		return result, nil
	}
	info, err := installEntry(root, old.Path)
	if err != nil || info == nil || !info.Mode().IsRegular() || info.Size() != f.Size {
		return result, err
	}
	valid, err := verifyDigest(ctx, root, old.Path, f)
	if err != nil || !valid {
		return result, err
	}
	if !result.cache.valid {
		result.cache.bytes = 0
	}
	result.cache.assemble = false
	result.status, result.local = "local_copy", old.Path
	return result, nil
}

func checkInParallel(ctx context.Context, count, workers int, out io.Writer, check func(context.Context, int) error) error {
	if count == 0 {
		return ctx.Err()
	}
	if workers == 0 {
		workers = defaultVerifyWorkers
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	var group sync.WaitGroup
	var next, checked atomic.Int64
	var first sync.Once
	var failure error
	for range min(workers, count) {
		group.Go(func() {
			for work.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= count {
					return
				}
				if err := check(work, i); err != nil {
					first.Do(func() {
						failure = err
						cancel()
					})
					return
				}
				checked.Add(1)
			}
		})
	}
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	var ticks <-chan time.Time
	if out != nil {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-done:
			if err := ctx.Err(); err != nil {
				return err
			}
			return failure
		case <-ticks:
			fmt.Fprintf(out, "Checking files %d/%d\n", checked.Load(), count)
		}
	}
}

func showPlan(out io.Writer, p plan) {
	fmt.Fprintf(out, "Build %s\n", p.Build)
	local := 0
	for _, job := range p.Jobs {
		if job.Local != "" {
			local++
		}
	}
	fmt.Fprintf(out, "Files to download %d  ·  Already verified %d\n", len(p.Jobs)-local, p.ReuseFiles)
	if local != 0 {
		fmt.Fprintf(out, "Files to copy locally %d\n", local)
	}
	fmt.Fprintf(out, "Download %s  ·  Free space needed %s\n", humanBytes(p.DownloadBytes), humanBytes(p.RequiredBytes))
	if p.UnpackPending {
		fmt.Fprintln(out, "Installation space will be checked after download.")
	}
}

func inspectFiles(ctx context.Context, out io.Writer, list manifest, dir, command string, asJSON bool, options downloadOptions) error {
	root, err := os.OpenRoot(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if root != nil {
		defer root.Close()
		if info, err := installEntry(root, installState); err != nil {
			return err
		} else if info != nil {
			return errors.New("an interrupted install is pending. Run download to recover it")
		}
	}
	if command == "verify" {
		options = downloadOptions{verifyWorkers: options.verifyWorkers, summary: options.summary}
	}
	if err := options.loadPrevious(root); err != nil {
		return err
	}
	var p plan
	var progress io.Writer
	if options.summary && !asJSON {
		progress = out
	}
	if list.Bundle == nil {
		p, err = planDownload(ctx, root, list, options, progress)
	} else {
		p, err = inspectArchive(ctx, root, list, options, command, progress)
	}
	if err != nil {
		return err
	}
	if command == "plan" {
		if asJSON {
			encoder := json.NewEncoder(out)
			encoder.SetIndent("", "  ")
			return encoder.Encode(p)
		}
		showPlan(out, p)
		return nil
	}
	var missing int
	for _, job := range p.Jobs {
		if job.Status == "missing" {
			missing++
		}
	}
	if options.summary {
		fmt.Fprintf(out, "Files %d  ·  Verified %d\nMissing %d  ·  Damaged %d\n", p.TotalFiles, p.ReuseFiles, missing, len(p.Jobs)-missing)
	}
	if len(p.Jobs) == 0 {
		fmt.Fprintln(out, "Game files verified.")
		return nil
	}
	return fmt.Errorf("repair needed (%d missing, %d damaged). Run the downloader to repair the files", missing, len(p.Jobs)-missing)
}

func preflightDownload(ctx context.Context, out io.Writer, root *os.Root, p plan) error {
	if err := clearAbandonedAssemblies(ctx, root); err != nil {
		return err
	}
	if err := verifyFreeSpace(root, p.RequiredBytes); err != nil {
		return err
	}
	if len(p.Jobs) != 0 {
		fmt.Fprintf(out, "Preparing %d files  ·  %s to download\n", len(p.Jobs), humanBytes(p.DownloadBytes))
	}
	return nil
}

func verifyFreeSpace(root *os.Root, required int64) error {
	dir, err := root.Open(stageDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	var disk syscall.Statfs_t
	if err := syscall.Fstatfs(int(dir.Fd()), &disk); err != nil {
		return fmt.Errorf("could not check free disk space: %w", err)
	}
	free := disk.Bavail * uint64(disk.Bsize)
	if uint64(required) > free {
		return fmt.Errorf("not enough disk space (%s needed, %s available)", humanBytes(required), humanBytes(int64(free)))
	}
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func preserved(name string, rules []string) bool {
	for _, rule := range rules {
		rule = strings.ToLower(rule)
		if name == strings.TrimSuffix(rule, "/") || (strings.HasSuffix(rule, "/") && strings.HasPrefix(name, rule)) {
			return true
		}
	}
	return false
}

func obsoleteDir(previous string) string {
	build := sha256.Sum256([]byte(previous))
	return fmt.Sprintf("%s/obsolete/%x", stageDir, build[:8])
}

func obsoleteUnchanged(ctx context.Context, root *os.Root, f resource) (bool, error) {
	info, err := installEntry(root, f.Path)
	if err != nil || info == nil || !info.Mode().IsRegular() || info.Size() != f.Size {
		return false, err
	}
	return verifyDigest(ctx, root, f.Path, f)
}

type pathTransition struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type pathUpdate struct {
	roots     map[string]pathTransition
	oldFiles  map[string]resource
	oldDirs   map[string]bool
	protected []string
	previous  string
}

func planPathUpdate(ctx context.Context, root *os.Root, current manifest, previous *manifest) (*pathUpdate, error) {
	if root == nil || previous == nil {
		return nil, nil
	}
	layout := &pathUpdate{
		roots:     map[string]pathTransition{},
		oldFiles:  make(map[string]resource, len(previous.Files)),
		oldDirs:   map[string]bool{},
		protected: append(append([]string(nil), previous.Preserve...), current.Preserve...),
		previous:  previous.Build,
	}
	for _, f := range previous.Files {
		key := strings.ToLower(f.Path)
		layout.oldFiles[key] = f
		for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
			layout.oldDirs[parent] = true
		}
	}
	checked := map[string]bool{}
	check := func(name string, wasDir bool) error {
		key := strings.ToLower(name)
		if checked[key] {
			return nil
		}
		checked[key] = true
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() != wasDir {
			return nil
		}
		change := pathTransition{Path: name, Kind: "file_to_directory"}
		if wasDir {
			change.Kind = "directory_to_file"
		}
		if err := layout.verifyOld(ctx, root, change); err != nil {
			return err
		}
		layout.roots[key] = change
		return nil
	}
	for _, f := range current.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A former file can block Stat on every descendant. Find that
		// transition before probing the proposed files under it.
		blocked := false
		for parent := path.Dir(f.Path); parent != "."; parent = path.Dir(parent) {
			key := strings.ToLower(parent)
			if _, wasFile := layout.oldFiles[key]; wasFile {
				if err := check(parent, false); err != nil {
					return nil, err
				}
				_, blocked = layout.roots[key]
				if blocked {
					break
				}
			}
		}
		if !blocked && layout.oldDirs[strings.ToLower(f.Path)] {
			if err := check(f.Path, true); err != nil {
				return nil, err
			}
		}
	}
	return layout, nil
}

func (u *pathUpdate) list() []pathTransition {
	if u == nil {
		return nil
	}
	result := make([]pathTransition, 0, len(u.roots))
	for _, change := range u.roots {
		result = append(result, change)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func (u *pathUpdate) at(name string) string {
	if u == nil {
		return ""
	}
	for key := strings.ToLower(name); key != "."; key = path.Dir(key) {
		if change, ok := u.roots[key]; ok {
			return change.Path
		}
	}
	return ""
}

func (u *pathUpdate) verifyOld(ctx context.Context, root *os.Root, change pathTransition) error {
	verifyFile := func(name string) error {
		key := strings.ToLower(name)
		old, known := u.oldFiles[key]
		if !known || preserved(key, u.protected) {
			return fmt.Errorf("the update is blocked by an untracked or protected path %q", name)
		}
		valid, err := obsoleteUnchanged(ctx, root, resource{
			Path: name, Size: old.Size, SHA256: old.SHA256, MD5: old.MD5,
		})
		if err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("the update is blocked by a modified file %q", name)
		}
		return nil
	}
	if change.Kind == "file_to_directory" {
		return verifyFile(change.Path)
	}
	return fs.WalkDir(root.FS(), change.Path, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			return verifyFile(name)
		}
		key := strings.ToLower(name)
		if !u.oldDirs[key] || preserved(key, u.protected) {
			return fmt.Errorf("the update is blocked by an untracked or protected folder %q", name)
		}
		return nil
	})
}
