package main

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	installState  = stageDir + "/install.json"
	installRecord = stageDir + "/installed.json"
	installBackup = stageDir + "/install-backup"
	installRegion = stageDir + "/region.json"
	installLock   = stageDir + "/lock"
	cnLaunchEntry = "AzurPromilia.exe"
	jpLaunchEntry = "AzurPromiliaJP.exe"
)

type stagedUpdateFile struct {
	path, staging string
}

type installPlan struct {
	layout *pathUpdate
	files  []stagedUpdateFile
	dirs   []string
}

// A step with no source creates an empty directory. Other steps rename a path.
type installStep struct {
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

func installEntry(root *os.Root, name string) (os.FileInfo, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	return info, err
}

func (p installPlan) commit(ctx context.Context, out io.Writer, root *os.Root) error {
	steps, err := p.steps(ctx, root)
	if err != nil || len(steps) == 0 {
		return err
	}
	if err := writeInstallJSON(root, installState, steps); err != nil {
		return err
	}
	for i, step := range steps {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, rollbackInstall(root, steps[:i]))
		}
		if err := applyInstallStep(root, step); err != nil {
			return errors.Join(fmt.Errorf("could not install %q: %w", step.To, err), rollbackInstall(root, steps[:i]))
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, rollbackInstall(root, steps))
	}
	if err := root.Remove(installState); err != nil {
		return errors.Join(err, rollbackInstall(root, steps))
	}
	if err := root.RemoveAll(installBackup); err != nil {
		fmt.Fprintln(out, "Installed files, but could not clean up the temporary backups.", err)
	}
	return nil
}

func (p installPlan) steps(ctx context.Context, root *os.Root) ([]installStep, error) {
	var steps []installStep
	backup := func(from, to string) error {
		info, err := installEntry(root, to)
		if err != nil {
			return err
		}
		if info != nil {
			return fmt.Errorf("a backup already exists at %q", to)
		}
		if err := root.MkdirAll(path.Dir(to), 0700); err != nil {
			return err
		}
		steps = append(steps, installStep{From: from, To: to})
		return nil
	}
	for _, change := range p.layout.list() {
		if err := p.layout.verifyOld(ctx, root, change); err != nil {
			return nil, err
		}
		if err := backup(change.Path, obsoleteDir(p.layout.previous)+"/"+change.Path); err != nil {
			return nil, err
		}
	}

	dirs := make(map[string]bool)
	addDirs := func(name string) error {
		for ; name != "." && !dirs[name]; name = path.Dir(name) {
			if p.layout.at(name) == "" {
				info, err := root.Stat(name)
				if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
					return err
				}
				if info != nil {
					if !info.IsDir() {
						return fmt.Errorf("%q is not a directory", name)
					}
					break
				}
			}
			dirs[name] = true
		}
		return nil
	}
	for i, f := range p.files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if size, err := regularSize(root, f.staging); err != nil || size < 0 {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("staged file for %q is missing", f.path)
		}
		if err := addDirs(path.Dir(f.path)); err != nil {
			return nil, err
		}
		if p.layout.at(f.path) != "" {
			continue
		}
		info, err := installEntry(root, f.path)
		if err != nil {
			return nil, err
		}
		if info != nil {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%q is not a regular file", f.path)
			}
			if err := backup(f.path, fmt.Sprintf("%s/%d", installBackup, i)); err != nil {
				return nil, err
			}
		}
	}
	for _, name := range p.dirs {
		if err := addDirs(name); err != nil {
			return nil, err
		}
	}
	ordered := make([]string, 0, len(dirs))
	for name := range dirs {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		steps = append(steps, installStep{To: name})
	}
	for _, f := range p.files {
		steps = append(steps, installStep{From: f.staging, To: f.path})
	}
	return steps, ctx.Err()
}

func applyInstallStep(root *os.Root, step installStep) error {
	if step.From == "" {
		return root.Mkdir(step.To, 0755)
	}
	info, err := installEntry(root, step.To)
	if err != nil {
		return err
	}
	if info != nil {
		return os.ErrExist
	}
	return root.Rename(step.From, step.To)
}

func writeInstallJSON(root *os.Root, name string, value any) error {
	temp := name + ".tmp"
	if err := root.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	writeErr := json.NewEncoder(file).Encode(value)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return root.Rename(temp, name)
}

func recoverInstall(root *os.Root) error {
	file, err := root.Open(installState)
	if errors.Is(err, os.ErrNotExist) {
		return root.RemoveAll(installBackup)
	}
	if err != nil {
		return err
	}
	var steps []installStep
	decodeErr := json.NewDecoder(file).Decode(&steps)
	if err := errors.Join(decodeErr, file.Close()); err != nil {
		return fmt.Errorf("could not read interrupted install state: %w", err)
	}
	if len(steps) == 0 {
		return errors.New("interrupted install state has no steps")
	}
	for _, step := range steps {
		if step.To == "." || !fs.ValidPath(step.To) || step.From == "." ||
			(step.From != "" && (!fs.ValidPath(step.From) || step.From == step.To)) {
			return errors.New("invalid path in interrupted install state")
		}
	}
	return rollbackInstall(root, steps)
}

func rollbackInstall(root *os.Root, steps []installStep) error {
	for i := len(steps) - 1; i >= 0; i-- {
		step := steps[i]
		if step.From == "" {
			info, err := installEntry(root, step.To)
			if err != nil {
				return err
			}
			if info != nil && info.IsDir() {
				if err := root.Remove(step.To); err != nil {
					return err
				}
			}
			continue
		}
		info, err := installEntry(root, step.From)
		if err != nil {
			return err
		}
		if info == nil {
			if err := root.Rename(step.To, step.From); err != nil {
				return fmt.Errorf("could not restore %q from %q: %w", step.From, step.To, err)
			}
		}
	}
	if err := root.Remove(installState); err != nil {
		return err
	}
	return root.RemoveAll(installBackup)
}

var errInstallRecord = errors.New("installation record is invalid")

type installation struct {
	manifest
	Archive string `json:"archive,omitempty"`
}

func readInstallRegion(root *os.Root, folder string) (gameRegion, error) {
	name := path.Join(folder, installRegion)
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := readBounded(file, 32)
	if err != nil {
		return "", err
	}
	var region gameRegion
	if json.Unmarshal(data, &region) != nil || !region.valid() {
		return "", fmt.Errorf("invalid installation region in %s", name)
	}
	return region, nil
}

func checkInstallRegion(root *os.Root, region gameRegion) (needsRegion bool, err error) {
	installed, err := readInstallRegion(root, ".")
	if err != nil {
		return false, err
	}
	if installed != "" && installed != region {
		return false, fmt.Errorf("this directory is for region %s, use a separate --dir for %s", installed, region)
	}
	other, launchEntry := regionJP, jpLaunchEntry
	if region == regionJP {
		other, launchEntry = regionCN, cnLaunchEntry
	}
	if info, err := installEntry(root, launchEntry); err != nil {
		return false, err
	} else if info != nil {
		return false, fmt.Errorf("found region %s file %q, use a separate --dir for %s", other, launchEntry, region)
	}
	if installed == "" && region == regionJP {
		// Staging files from before region selection belong to CN.
		entries, err := fs.ReadDir(root.FS(), stageDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		for _, entry := range entries {
			if entry.Name() != path.Base(installLock) && entry.Name() != path.Base(installRegion)+".tmp" {
				return false, errors.New("this directory has existing CN download state, use a separate --dir for jp")
			}
		}
	}
	return installed == "", nil
}

func bundleHome(list manifest) string {
	id := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", list.Build, list.Bundle.Size, list.Bundle.MD5)))
	return fmt.Sprintf("%s/archives/%x", stageDir, id)
}

func (opts *downloadOptions) loadPrevious(root *os.Root) error {
	installed, err := readInstallRecord(root, installRecord)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errInstallRecord) {
		return nil
	}
	if err != nil {
		return err
	}
	opts.previous = &installed.manifest
	return nil
}

func readInstallRecord(root *os.Root, name string) (installation, error) {
	var installed installation
	if root == nil {
		return installed, os.ErrNotExist
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return installed, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return installed, err
	}
	if !info.Mode().IsRegular() {
		return installed, errInstallRecord
	}
	data, err := readBounded(file, 64<<20)
	if err != nil {
		return installed, err
	}
	if err := json.Unmarshal(data, &installed); err != nil {
		return installed, fmt.Errorf("%w (%v)", errInstallRecord, err)
	}
	if installed.Bundle != nil {
		return installed, errInstallRecord
	}
	if err := installed.validateFiles(false); err != nil {
		return installed, fmt.Errorf("%w (%v)", errInstallRecord, err)
	}
	return installed, nil
}

func scanInstalledVersions(ctx context.Context, root *os.Root, launchEntry string, region gameRegion) (map[string]string, error) {
	versions := make(map[string]string)
	launchEntry = strings.ReplaceAll(launchEntry, "\\", "/")
	if !validPath(launchEntry) {
		launchEntry = cnLaunchEntry
	}
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		folder := path.Dir(name)
		switch entry.Name() {
		case stageDir:
			record, err := readInstallRecord(root, path.Join(folder, installRecord))
			if err == nil {
				installed, err := readInstallRegion(root, folder)
				if err != nil {
					return err
				}
				if installed == region || (installed == "" && region == regionCN) {
					versions[folder] = record.Build
				}
			} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errInstallRecord) {
				return err
			}
		case "_index":
			if region == regionCN && versions[folder] == "" {
				exe, err := installEntry(root, path.Join(folder, launchEntry))
				if err != nil {
					return err
				}
				if exe != nil && exe.Mode().IsRegular() {
					build, err := readLauncherVersion(root, path.Join(name, "version"))
					if err != nil {
						return err
					}
					if build != "" {
						versions[folder] = build
					}
				}
			}
		default:
			return nil
		}
		return fs.SkipDir
	})
	return versions, err
}

func readLauncherVersion(root *os.Root, name string) (string, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<10 {
		return "", err
	}
	data, err := readBounded(file, 4<<10)
	if err != nil {
		return "", err
	}
	var record struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &record) != nil || !validBuildID(record.Version) {
		return "", nil
	}
	return record.Version, nil
}

func readBundleReceipt(root *os.Root, list manifest) (manifest, error) {
	home := bundleHome(list)
	installed, err := readInstallRecord(root, installRecord)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errInstallRecord) {
		return manifest{}, err
	}
	if err != nil || installed.Archive != home || installed.Build != list.Build {
		// Older builds stored one record per archive.
		installed, err = readInstallRecord(root, home+"/installed.json")
		if err != nil {
			return manifest{}, err
		}
		if installed.Build != list.Build {
			return manifest{}, errInstallRecord
		}
	}
	installed.Preserve = list.Preserve
	return installed.manifest, nil
}

func inspectArchive(ctx context.Context, root *os.Root, list manifest, opts downloadOptions, command string, progress io.Writer) (plan, error) {
	installed, err := readBundleReceipt(root, list)
	if err == nil {
		p, err := planDownload(ctx, root, installed, downloadOptions{previous: opts.previous, verifyWorkers: opts.verifyWorkers}, progress)
		if err != nil || command == "verify" || len(p.Jobs) == 0 {
			return p, err
		}
	} else if !errors.Is(err, os.ErrNotExist) && (command == "verify" || !errors.Is(err, errInstallRecord)) {
		return plan{}, err
	} else if command == "verify" {
		return plan{}, errors.New("verification is available after installation")
	}
	var stage *os.Root
	if root != nil {
		stage, err = root.OpenRoot(bundleHome(list) + "/chunks")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return plan{}, err
		}
	}
	if stage != nil {
		defer stage.Close()
	}
	return archiveDownloadPlan(ctx, stage, list, opts, progress)
}

func archiveDownloadPlan(ctx context.Context, stage *os.Root, list manifest, opts downloadOptions, progress io.Writer) (plan, error) {
	opts.previous = nil
	p, err := planDownload(ctx, stage, list, opts, progress)
	p.UnpackPending = true
	return p, err
}

func downloadArchive(ctx context.Context, out io.Writer, client *http.Client, root *os.Root, list manifest, opts downloadOptions) error {
	home := bundleHome(list)
	if installed, err := readBundleReceipt(root, list); err == nil {
		p, err := planDownload(ctx, root, installed, downloadOptions{previous: opts.previous, verifyWorkers: opts.verifyWorkers}, out)
		if err != nil {
			return err
		}
		if len(p.Jobs) == 0 {
			receipt, err := stageInstallRecord(root, installed, home)
			if err != nil {
				return err
			}
			if err := (installPlan{files: []stagedUpdateFile{receipt}}).commit(ctx, out, root); err != nil {
				return err
			}
			fmt.Fprintf(out, "All %d files are up to date.\n", p.ReuseFiles)
			cleanArchive(out, root, home)
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errInstallRecord) {
		return err
	}

	if err := root.MkdirAll(home+"/chunks/"+stageDir, 0700); err != nil {
		return err
	}
	stage, err := root.OpenRoot(home + "/chunks")
	if err != nil {
		return err
	}
	defer stage.Close()
	p, err := archiveDownloadPlan(ctx, stage, list, opts, out)
	if err != nil {
		return err
	}
	if err := preflightDownload(ctx, out, stage, p); err != nil {
		return err
	}
	if err := downloadPlanned(ctx, out, client, stage, p, opts, false); err != nil {
		return err
	}

	fmt.Fprintln(out, "Checking the download...")
	reader := newZIPParts(stage, list.Files)
	defer reader.close()
	sum := md5.New()
	if _, err := io.Copy(sum, contextReader{ctx, io.NewSectionReader(reader, 0, list.Bundle.Size)}); err != nil {
		return err
	}
	if fmt.Sprintf("%x", sum.Sum(nil)) != list.Bundle.MD5 {
		return errors.New("the archive failed its checksum check")
	}
	archive, err := zip.NewReader(reader, list.Bundle.Size)
	if err != nil {
		return fmt.Errorf("could not open the archive: %w", err)
	}
	unpack, err := prepareZIP(ctx, root, archive, list, opts, out)
	if err != nil {
		return err
	}
	staged := home + "/extract.part"
	if err := root.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.RemoveAll(home + "/layout"); err != nil {
		return err
	}
	if err := verifyFreeSpace(root, unpack.bytes); err != nil {
		return err
	}
	installed := manifest{Build: list.Build, Preserve: list.Preserve}
	fmt.Fprintf(out, "Installing %d files (%s)\n", len(unpack.entries), humanBytes(unpack.bytes))
	last := time.Now()
	var changes []stagedUpdateFile
	for i, entry := range unpack.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(last) >= time.Second {
			fmt.Fprintf(out, "Installing files %d/%d\n", i, len(unpack.entries))
			last = time.Now()
		}
		f := entry.resource
		if f.SHA256 == "" {
			destination := home + "/layout/" + f.Path
			changes = append(changes, stagedUpdateFile{f.Path, destination})
			f.SHA256, err = extractZIPFile(ctx, root, staged, destination, entry)
			if err != nil {
				return fmt.Errorf("%s：%w", f.Path, err)
			}
		}
		installed.Files = append(installed.Files, f)
	}
	receipt, err := stageInstallRecord(root, installed, home)
	if err != nil {
		return err
	}
	changes = append(changes, receipt)
	if err := (installPlan{layout: unpack.update, files: changes, dirs: unpack.dirs}).commit(ctx, out, root); err != nil {
		return err
	}
	if err := errors.Join(reader.close(), stage.Close()); err != nil {
		fmt.Fprintln(out, "Installed files, but could not close the archive cache.", err)
	}
	cleanArchive(out, root, home)
	return nil
}

func cleanArchive(out io.Writer, root *os.Root, home string) {
	for _, folder := range []string{"/chunks", "/layout"} {
		if err := root.RemoveAll(home + folder); err != nil {
			fmt.Fprintln(out, "Installed files, but could not clean up the archive cache.", err)
		}
	}
}

type zipEntry struct {
	file *zip.File
	resource
}

type unpackPlan struct {
	entries []zipEntry
	dirs    []string
	bytes   int64
	update  *pathUpdate
}

func prepareZIP(ctx context.Context, root *os.Root, archive *zip.Reader, list manifest, opts downloadOptions, progress io.Writer) (unpackPlan, error) {
	var p unpackPlan
	target := manifest{Build: list.Build, Preserve: list.Preserve}
	files := make(map[string]bool, len(archive.File))
	paths := make(map[string]string, len(archive.File))
	for _, file := range archive.File {
		mode := file.Mode()
		name := strings.TrimSuffix(file.Name, "/")
		if !validPath(name) || (!mode.IsRegular() && !mode.IsDir()) {
			return unpackPlan{}, fmt.Errorf("invalid path or file type in archive: %q", file.Name)
		}
		key := strings.ToLower(name)
		if _, exists := files[key]; exists {
			return unpackPlan{}, fmt.Errorf("duplicate path in archive: %q", name)
		}
		files[key] = mode.IsRegular()
		for parent := name; parent != "."; parent = path.Dir(parent) {
			lower := strings.ToLower(parent)
			if existing, ok := paths[lower]; ok && existing != parent {
				return unpackPlan{}, fmt.Errorf("paths in archive differ only by case: %q and %q", existing, parent)
			}
			paths[lower] = parent
		}
		if mode.IsDir() {
			p.dirs = append(p.dirs, name)
		} else {
			if file.UncompressedSize64 >= math.MaxInt64 {
				return unpackPlan{}, fmt.Errorf("file in archive is too large: %q", name)
			}
			f := resource{Path: name, Size: int64(file.UncompressedSize64)}
			p.entries = append(p.entries, zipEntry{file, f})
			target.Files = append(target.Files, f)
		}
	}
	if len(p.entries) == 0 {
		return unpackPlan{}, errors.New("the archive contains no game files")
	}
	for _, name := range paths {
		if files[strings.ToLower(path.Dir(name))] {
			return unpackPlan{}, fmt.Errorf("a directory in the archive is also listed as a file: %q", name)
		}
	}

	var err error
	p.update, err = planPathUpdate(ctx, root, target, opts.previous)
	if err != nil {
		return unpackPlan{}, err
	}
	if err := checkInParallel(ctx, len(p.entries), opts.verifyWorkers, progress, func(ctx context.Context, i int) error {
		f := &p.entries[i]
		if p.update.at(f.Path) == "" {
			size, err := regularSize(root, f.Path)
			if err != nil {
				return err
			}
			if size == f.Size {
				expected, err := copyZIPFile(ctx, io.Discard, *f)
				if err != nil {
					return err
				}
				actual, err := sha256File(ctx, root, f.Path)
				if err != nil {
					return err
				}
				if actual == expected {
					f.SHA256 = actual
				}
			}
		}
		return nil
	}); err != nil {
		return unpackPlan{}, err
	}
	for _, f := range p.entries {
		if f.SHA256 != "" {
			continue
		}
		if f.Size > math.MaxInt64-p.bytes {
			return unpackPlan{}, errors.New("the extracted archive would be too large")
		}
		p.bytes += f.Size
	}
	return p, nil
}

func extractZIPFile(ctx context.Context, root *os.Root, staged, destination string, entry zipEntry) (string, error) {
	if err := root.MkdirAll(path.Dir(destination), 0755); err != nil {
		return "", err
	}
	out, err := root.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer root.Remove(staged)
	digest, err := copyZIPFile(ctx, out, entry)
	closeErr := out.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	return digest, root.Rename(staged, destination)
}

func copyZIPFile(ctx context.Context, out io.Writer, entry zipEntry) (string, error) {
	in, err := entry.file.Open()
	if err != nil {
		return "", err
	}
	defer in.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(contextReader{ctx, in}, entry.Size+1))
	if err != nil {
		return "", err
	}
	if n != entry.Size {
		return "", errors.New("an extracted file has the wrong size")
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func stageInstallRecord(root *os.Root, list manifest, archive string) (stagedUpdateFile, error) {
	temp := installRecord + ".tmp"
	if err := root.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return stagedUpdateFile{}, err
	}
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return stagedUpdateFile{}, err
	}
	// Retain only paths and checksums needed to track installed files.
	files := make([]resource, len(list.Files))
	copy(files, list.Files)
	for i := range files {
		files[i].URL, files[i].Mirrors = "", nil
	}
	list.Files = files
	encodeErr := json.NewEncoder(file).Encode(installation{manifest: list, Archive: archive})
	if err := errors.Join(encodeErr, file.Close()); err != nil {
		root.Remove(temp)
		return stagedUpdateFile{}, err
	}
	return stagedUpdateFile{path: installRecord, staging: temp}, nil
}

// zipParts supplies one seekable view over the ordered slices. Keeping only the
// current file open works even when a release contains thousands of slices.
type zipParts struct {
	root    *os.Root
	names   []string
	ends    []int64
	mu      sync.Mutex
	current int
	file    *os.File
}

func newZIPParts(root *os.Root, files []resource) *zipParts {
	r := &zipParts{root: root, current: -1}
	var end int64
	for _, file := range files {
		end += file.Size
		r.names = append(r.names, file.Path)
		r.ends = append(r.ends, end)
	}
	return r
}

func (r *zipParts) ReadAt(dst []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if off < 0 {
		return 0, errors.New("invalid archive read offset")
	}
	start := sort.Search(len(r.ends), func(i int) bool { return r.ends[i] > off })
	var n int
	for i := start; len(dst) != 0 && i < len(r.ends); i++ {
		if r.current != i {
			if err := r.close(); err != nil {
				return n, err
			}
			file, err := r.root.Open(r.names[i])
			if err != nil {
				return n, err
			}
			r.file, r.current = file, i
		}
		pieceStart := int64(0)
		if i > 0 {
			pieceStart = r.ends[i-1]
		}
		take := int(min(int64(len(dst)), r.ends[i]-off))
		read, err := r.file.ReadAt(dst[:take], off-pieceStart)
		n += read
		if err != nil {
			return n, err
		}
		dst, off = dst[read:], off+int64(read)
	}
	if len(dst) > 0 {
		return n, io.EOF
	}
	return n, nil
}

func (r *zipParts) close() error {
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file, r.current = nil, -1
	return err
}
