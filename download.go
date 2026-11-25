package main

import (
	"cmp"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"hash/crc64"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const stageDir = ".azur-download"

const (
	maxDownloadWorkers     = 10
	defaultWorkers         = maxDownloadWorkers
	maxConcurrentTransfers = 16
	defaultVerifyWorkers   = 8
	maxVerifyWorkers       = 64
	defaultParts           = 4
	defaultPartSize        = int64(32 << 20)
)

type downloadOptions struct {
	previous                *manifest
	stageChecks             *stageChecks
	workers, retries, parts int
	verifyWorkers           int
	partSize, limit         int64
	summary                 bool
}

type downloader struct {
	client    *http.Client
	root      *os.Root
	limit     *rateLimit
	retries   *retryWindows
	transfers chan struct{}
	options   downloadOptions
}

var (
	errNoRanges = errors.New("the server does not support ranged downloads")
	errChecksum = errors.New("file verification failed")
)

type filePart struct {
	path       string
	start, end int64
}

type progressWriter struct {
	dst   *os.File
	saved *atomic.Int64
}

func (w progressWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.saved.Add(int64(n))
	return n, err
}

func download(ctx context.Context, out io.Writer, client *http.Client, list manifest, dir string, region gameRegion, options downloadOptions) (err error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := checkInstallRegion(root, region); err != nil {
		return err
	}
	if err := root.MkdirAll(stageDir, 0700); err != nil {
		return err
	}
	lock, err := root.OpenFile(installLock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("another download is using this folder")
		}
		return err
	}
	needsRegion, err := checkInstallRegion(root, region)
	if err != nil {
		return err
	}
	if needsRegion {
		if err := writeInstallJSON(root, installRegion, region); err != nil {
			return err
		}
	}
	if err := recoverInstall(root); err != nil {
		return err
	}
	if err := options.loadPrevious(root); err != nil {
		return err
	}
	if list.Bundle != nil {
		return downloadArchive(ctx, out, client, root, list, options)
	}
	options.stageChecks = loadStageChecks(root)
	defer func() {
		if err != nil {
			options.stageChecks.save(root)
		}
	}()
	p, err := planDownload(ctx, root, list, options, out)
	if err != nil {
		return err
	}
	if err := preflightDownload(ctx, out, root, p); err != nil {
		return err
	}

	if len(p.Jobs) > 0 {
		if err := downloadPlanned(ctx, out, client, root, p, options, true); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "All %d files are up to date.\n", p.ReuseFiles)
	}
	receipt, err := stageInstallRecord(root, list, "")
	if err != nil {
		return err
	}
	staged := make([]stagedUpdateFile, 0, len(p.Jobs)+1)
	for _, job := range p.Jobs {
		staged = append(staged, stagedUpdateFile{job.Path, partPath(job.File)})
	}
	staged = append(staged, receipt)
	if err := (installPlan{layout: p.update, files: staged}).commit(ctx, out, root); err != nil {
		return err
	}
	_ = root.Remove(stageChecksPath)
	return nil
}

func downloadPlanned(ctx context.Context, out io.Writer, client *http.Client, root *os.Root, p plan, options downloadOptions, stageOnly bool) error {
	workers := min(options.workers, len(p.Jobs))
	transferSlots := max(workers, maxConcurrentTransfers)
	d := downloader{
		client: client, root: root, options: options,
		limit:   &rateLimit{bytesPerSecond: options.limit},
		retries: &retryWindows{until: make(map[string]time.Time)},
		// Share request slots between whole files and their range requests.
		transfers: make(chan struct{}, transferSlots),
	}
	var large, regular fileQueue
	for i := range p.Jobs {
		job := &p.Jobs[i]
		if job.Bytes > 0 && canSplitFile(job.File, options) {
			large.jobs = append(large.jobs, job)
		} else {
			regular.jobs = append(regular.jobs, job)
		}
	}
	slices.SortStableFunc(large.jobs, func(a, b *planFile) int { return cmp.Compare(b.Bytes, a.Bytes) })
	// Each split file needs up to parts-1 extra request slots. Reserve capacity for whole files.
	largeWorkers := min(len(large.jobs), max(1, workers-1), max(1, (transferSlots-workers)/max(1, options.parts-1)))
	progress := newDownloadProgress(p, workers)
	panel := newDownloadPanel(out)
	interval := time.Second
	if panel != nil {
		interval = 500 * time.Millisecond
	}
	var group sync.WaitGroup
	for i := range progress.slots {
		slot := &progress.slots[i]
		first, second := &regular, &large
		if i < largeWorkers {
			first, second = second, first
		}
		group.Go(func() {
			for ctx.Err() == nil {
				job := first.take()
				if job == nil {
					job = second.take()
				}
				if job == nil {
					return
				}
				progress.start(slot, job)
				err := d.file(ctx, *job, stageOnly, &slot.fileProgress)
				progress.finish(slot, err, ctx.Err())
			}
		})
	}
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	rate := transferRate{at: time.Now()}
	active := make([]activeDownload, workers)
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			stats := progress.snapshot(active)
			rate.sample(now, stats.received)
			if panel != nil && panel.draw(out, now, p, active, stats, rate.speed) {
				continue
			}
			showProgress(out, stats.completed+p.ReuseFiles, p.TotalFiles, stats.saved, p.TotalBytes, rate.speed)
		case <-done:
			if !panel.stop() {
				clearProgress(out)
			}
			completed, failed := progress.totals.completed, progress.totals.failed
			if options.summary {
				label := "Files"
				if p.UnpackPending {
					label = "Archive pieces"
				}
				fmt.Fprintf(out, "%s %d  ·  Verified %d  ·  Staged %d\nFailed %d  ·  Unprocessed %d\n",
					label, p.TotalFiles, p.ReuseFiles, completed, failed, len(p.Jobs)-completed-failed)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if failed != 0 {
				return fmt.Errorf("download failed for %d files (%w)", failed, progress.firstFailure)
			}
			return nil
		}
	}
}

type fileQueue struct {
	jobs []*planFile
	next atomic.Int64
}

func (q *fileQueue) take() *planFile {
	index := q.next.Add(1) - 1
	if index >= int64(len(q.jobs)) {
		return nil
	}
	return q.jobs[index]
}

func (d downloader) file(ctx context.Context, job planFile, stageOnly bool, progress *fileProgress) error {
	f := job.File
	if !stageOnly {
		if err := d.root.MkdirAll(path.Dir(f.Path), 0755); err != nil {
			return err
		}
	}
	part := partPath(f)
	if job.checkedStage != nil {
		info, err := regularFile(d.root, part)
		if err != nil {
			return err
		}
		if sameStagedFile(job.checkedStage, info) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if job.stageValid {
				return d.commit(f, part, stageOnly)
			}
			if err := d.discard(f, part); err != nil {
				return err
			}
		}
		progress.saved.Store(0)
	}
	if job.Local != "" {
		copied, err := d.stageLocal(ctx, f, part, job.Local)
		if err != nil {
			return err
		}
		if copied {
			progress.saved.Store(f.Size)
			return d.commit(f, part, stageOnly)
		}
		progress.localFallback.Store(true)
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := f
		if selected := attempt % (1 + len(f.Mirrors)); selected > 0 {
			source.URL = f.Mirrors[selected-1]
		}
		retry, err := d.attempt(ctx, source, part, stageOnly, progress)
		if err == nil || !retry || attempt >= d.options.retries {
			return err
		}
		if err := wait(ctx, time.Duration(1<<min(attempt, 5))*250*time.Millisecond); err != nil {
			return err
		}
	}
}

func (d downloader) stageLocal(ctx context.Context, f resource, part, source string) (bool, error) {
	if valid, err := verify(ctx, d.root, part, f); valid || err != nil {
		return valid, err
	}
	in, err := d.root.Open(source)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer in.Close()
	temp := part + ".copying"
	if err := d.root.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	out, err := d.root.OpenFile(temp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	defer d.root.Remove(temp)
	digest := f.newHash()
	n, copyErr := copyBuffered(io.MultiWriter(out, digest), io.LimitReader(contextReader{ctx, in}, f.Size+1))
	if err := errors.Join(copyErr, out.Close()); err != nil {
		return false, err
	}
	if n != f.Size || !f.matchesHash(digest) {
		return false, nil
	}
	return true, d.root.Rename(temp, part)
}

func partPath(f resource) string {
	key := sha256.Sum256([]byte(f.Path + "\x00" + f.digest() + "\x00" + strconv.FormatInt(f.Size, 10)))
	return fmt.Sprintf("%s/%x.part", stageDir, key)
}

func (d downloader) attempt(ctx context.Context, f resource, part string, stageOnly bool, progress *fileProgress) (bool, error) {
	offset, err := regularSize(d.root, part)
	if err != nil {
		return false, err
	}
	if offset >= f.Size {
		if offset == f.Size {
			valid, err := verifyDigest(ctx, d.root, part, f)
			if err != nil {
				return false, err
			}
			if valid {
				progress.saved.Store(f.Size)
				return false, d.commit(f, part, stageOnly)
			}
		}
		if err := d.discard(f, part); err != nil {
			return false, err
		}
		progress.saved.Store(0)
		offset = -1
	}
	if offset > 0 {
		progress.saved.Store(offset)
	}
	retry, err := d.fetchFile(ctx, f, part, offset, progress)
	if errors.Is(err, errChecksum) {
		if err := d.discard(f, part); err != nil {
			return false, err
		}
		progress.saved.Store(0)
	}
	if err != nil {
		return retry, err
	}
	progress.saved.Store(f.Size)
	return false, d.commit(f, part, stageOnly)
}

func (d downloader) discard(f resource, name string) error {
	if err := d.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return d.removeFileParts(f)
}

func (d downloader) commit(f resource, staged string, stageOnly bool) error {
	if err := d.removeFileParts(f); err != nil {
		return err
	}
	if stageOnly {
		return nil
	}
	return d.root.Rename(staged, f.Path)
}

func canSplitFile(f resource, options downloadOptions) bool {
	return options.parts > 1 && f.Size > max(1, options.partSize)
}

func partLayouts(f resource, options downloadOptions) (current, legacy []filePart) {
	if !canSplitFile(f, options) {
		return nil, nil
	}
	// Shorter ranges let a freed connection pick up more data while its peers are still working.
	size := max(1, options.partSize/2, (f.Size-1)/int64(options.parts)/2+1)
	oldSize := max(options.partSize, (f.Size-1)/int64(options.parts)+1)
	stem := partPath(f)
	current = splitFile(stem, f.Size, size)
	if oldSize != size {
		legacy = splitFile(stem, f.Size, oldSize)
	}
	return current, legacy
}

func splitFile(stem string, total, size int64) []filePart {
	var pieces []filePart
	for start := int64(0); start < total; {
		end := start + min(size, total-start)
		pieces = append(pieces, filePart{fmt.Sprintf("%s.%d.%d", stem, size, start), start, end})
		start = end
	}
	return pieces
}

func resumableParts(root *os.Root, f resource, options downloadOptions) ([]filePart, int64, error) {
	current, legacy := partLayouts(f, options)
	saved, err := retainedParts(root, current)
	if err != nil {
		return nil, 0, err
	}
	previous, err := retainedParts(root, legacy)
	if err != nil {
		return nil, 0, err
	}
	if previous > saved {
		return legacy, previous, nil
	}
	return current, saved, nil
}

func retainedParts(root *os.Root, pieces []filePart) (int64, error) {
	var saved int64
	for _, piece := range pieces {
		n, err := regularSize(root, piece.path)
		if err != nil {
			return 0, err
		}
		if n > 0 && n <= piece.end-piece.start {
			saved += n
		}
	}
	return saved, nil
}

type stagedCache struct {
	checked         os.FileInfo
	bytes           int64
	assemble, valid bool
}

func checkCache(ctx context.Context, root *os.Root, f resource, options downloadOptions) (stagedCache, error) {
	part := partPath(f)
	info, err := regularFile(root, part)
	if err != nil {
		return stagedCache{}, err
	}
	if info != nil && info.Size() == f.Size {
		checks := options.stageChecks
		stamp := stampStagedFile(info)
		if !checks.contains(part, stamp) {
			valid, err := verifyDigest(ctx, root, part, f)
			if !valid || err != nil {
				return stagedCache{checked: info, assemble: canSplitFile(f, options)}, err
			}
		}
		if checks != nil {
			checks.record(part, stamp)
		}
		return stagedCache{checked: info, bytes: f.Size, valid: true}, nil
	}
	if info != nil && info.Size() > 0 && info.Size() < f.Size {
		return stagedCache{bytes: info.Size()}, nil
	}
	pieces, saved, err := resumableParts(root, f, options)
	return stagedCache{bytes: saved, assemble: len(pieces) > 1}, err
}

func sameStagedFile(before, after os.FileInfo) bool {
	return after != nil && stampStagedFile(before) == stampStagedFile(after)
}

func (d downloader) fetchFile(ctx context.Context, f resource, part string, offset int64, progress *fileProgress) (bool, error) {
	if offset <= 0 && canSplitFile(f, d.options) {
		pieces, _, err := resumableParts(d.root, f, d.options)
		if err != nil {
			return false, err
		}
		retry, err := d.fetchParts(ctx, f, part, pieces, progress)
		if !errors.Is(err, errNoRanges) {
			return retry, err
		}
		if err := d.removeParts(pieces); err != nil {
			return false, err
		}
		progress.saved.Store(0)
	}
	piece := filePart{part, 0, f.Size}
	retry, err := d.transfer(ctx, f, piece, max(offset, 0), progress)
	if offset > 0 && errors.Is(err, errNoRanges) {
		progress.saved.Store(0)
		return d.transfer(ctx, f, piece, 0, progress)
	}
	return retry, err
}

func (d downloader) fetchParts(ctx context.Context, f resource, part string, pieces []filePart, progress *fileProgress) (bool, error) {
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		retry bool
		err   error
	}
	results := make(chan outcome, len(pieces))
	concurrent := make(chan struct{}, min(d.options.parts, len(pieces)))
	for _, piece := range pieces {
		go func() {
			select {
			case concurrent <- struct{}{}:
				defer func() { <-concurrent }()
			case <-work.Done():
				results <- outcome{false, work.Err()}
				return
			}
			offset, err := regularSize(d.root, piece.path)
			if err != nil || offset == piece.end-piece.start {
				results <- outcome{false, err}
				return
			}
			if offset > piece.end-piece.start {
				if err := d.root.Remove(piece.path); err != nil {
					results <- outcome{false, err}
					return
				}
				offset = 0
			}
			retry, err := d.transfer(work, f, piece, max(offset, 0), progress)
			results <- outcome{retry, err}
		}()
	}
	var failure outcome
	for range pieces {
		r := <-results
		if r.err != nil && (failure.err == nil || errors.Is(r.err, errNoRanges)) {
			failure = r
			cancel()
		}
	}
	if failure.err != nil {
		return failure.retry, failure.err
	}

	assembly := part + ".assembling"
	out, err := d.root.OpenFile(assembly, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	defer d.root.Remove(assembly)
	digest := f.newHash()
	output := io.MultiWriter(out, digest)
	for _, piece := range pieces {
		in, err := d.root.Open(piece.path)
		if err != nil {
			out.Close()
			return false, err
		}
		size := piece.end - piece.start
		n, err := copyBuffered(output, io.LimitReader(contextReader{ctx, in}, size+1))
		in.Close()
		if err != nil {
			out.Close()
			return false, err
		}
		if n != size {
			out.Close()
			return true, errChecksum
		}
	}
	if err := out.Close(); err != nil {
		return false, err
	}
	if !f.matchesHash(digest) {
		return true, errChecksum
	}
	return false, d.root.Rename(assembly, part)
}

func (d downloader) removeFileParts(f resource) error {
	current, legacy := partLayouts(f, d.options)
	if err := d.removeParts(current); err != nil {
		return err
	}
	return d.removeParts(legacy)
}

func (d downloader) removeParts(pieces []filePart) error {
	for _, piece := range pieces {
		if err := d.root.Remove(piece.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (d downloader) transfer(ctx context.Context, f resource, piece filePart, offset int64, progress *fileProgress) (bool, error) {
	for {
		result, retry, err := d.transferRange(ctx, f, piece, offset, progress)
		if err != nil {
			return retry, err
		}
		if result.offset == piece.end-piece.start {
			if result.verified || piece.start != 0 || piece.end != f.Size {
				return false, nil
			}
			// A resumed response contains only the suffix. Check it together with the saved prefix.
			valid, err := verify(ctx, d.root, piece.path, f)
			if err != nil {
				return false, err
			}
			if !valid {
				return true, errChecksum
			}
			return false, nil
		}
		// A CDN can serve a valid prefix of the requested range.
		offset = result.offset
	}
}

type rangeResult struct {
	offset   int64
	verified bool
}

func (d downloader) transferRange(ctx context.Context, f resource, piece filePart, offset int64, progress *fileProgress) (rangeResult, bool, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, f.URL, nil)
	if err != nil {
		return rangeResult{}, false, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	split := piece.start != 0 || piece.end != f.Size
	if split {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", piece.start+offset, piece.end-1))
	} else if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	origin := req.URL.Scheme + "://" + strings.ToLower(req.URL.Host)
	if err := d.acquireTransfer(ctx, origin); err != nil {
		return rangeResult{}, false, err
	}
	defer func() { <-d.transfers }()
	resp, err := d.client.Do(req)
	if err != nil {
		return rangeResult{}, true, err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	end := piece.end
	restart := false
	switch resp.StatusCode {
	case http.StatusOK:
		if split {
			return rangeResult{}, false, errNoRanges
		}
		restart = offset > 0
		offset = 0
	case http.StatusPartialContent:
		got := resp.Header.Get("Content-Range")
		first, last, size, valid := parseContentRange(got)
		if !valid || (!split && offset == 0) || first != piece.start+offset || last >= piece.end || size != f.Size {
			return rangeResult{}, true, fmt.Errorf("invalid Content-Range %q (expected a range within %d-%d/%d)", got, piece.start+offset, piece.end-1, f.Size)
		}
		end = last + 1
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	case http.StatusRequestedRangeNotSatisfiable:
		if split || offset > 0 {
			return rangeResult{}, false, errNoRanges
		}
		return rangeResult{}, false, fmt.Errorf("HTTP %d", resp.StatusCode)
	default:
		code := resp.StatusCode
		if code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable {
			d.retries.record(origin, resp.Header.Get("Retry-After"))
		}
		retry := code == 408 || code == 429 || code >= 500 || (len(f.Mirrors) != 0 && (code == 403 || code == 404))
		return rangeResult{}, retry, fmt.Errorf("HTTP %d", code)
	}
	remaining := end - piece.start - offset
	if resp.ContentLength >= 0 && resp.ContentLength != remaining {
		return rangeResult{}, true, fmt.Errorf("server reported %d bytes, expected %d", resp.ContentLength, remaining)
	}
	dst, err := d.root.OpenFile(piece.path, flags, 0600)
	if err != nil {
		return rangeResult{}, false, err
	}
	if restart {
		progress.saved.Store(0)
	}
	idle := time.AfterFunc(time.Hour, cancel)
	idle.Stop()
	defer idle.Stop()
	body := responseReader{
		ctx: reqCtx, body: resp.Body, idle: idle, limit: d.limit,
		bytes: &progress.received,
	}
	var digest hash.Hash
	var output io.Writer = progressWriter{dst, &progress.saved}
	if resp.StatusCode == http.StatusOK {
		digest = f.newHash()
		output = io.MultiWriter(output, digest)
	}
	n, readErr := copyBuffered(output, io.LimitReader(body, remaining))
	if readErr == nil && n == remaining {
		var extra [1]byte
		if _, err := io.ReadFull(body, extra[:]); err == nil {
			readErr = fmt.Errorf("server sent more than the expected %d bytes", remaining)
		} else if !errors.Is(err, io.EOF) {
			readErr = err
		}
	}
	closeErr := dst.Close()
	if closeErr != nil {
		return rangeResult{}, false, closeErr
	}
	if readErr != nil {
		return rangeResult{}, true, readErr
	}
	if n != remaining {
		return rangeResult{}, true, fmt.Errorf("received %d bytes, expected %d", n, remaining)
	}
	if digest != nil && !f.matchesHash(digest) {
		return rangeResult{}, true, errChecksum
	}
	return rangeResult{offset: end - piece.start, verified: digest != nil}, false, nil
}

func (d downloader) acquireTransfer(ctx context.Context, origin string) error {
	for {
		if err := d.retries.wait(ctx, origin); err != nil {
			return err
		}
		select {
		case d.transfers <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		// A request may have received Retry-After while this transfer was queued.
		if d.retries.delay(origin) <= 0 {
			return nil
		}
		<-d.transfers
	}
}

func parseContentRange(value string) (first, last, size int64, valid bool) {
	fields := strings.Fields(value)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bytes") {
		return 0, 0, 0, false
	}
	bounds, total, _ := strings.Cut(fields[1], "/")
	from, to, _ := strings.Cut(bounds, "-")
	first, errFirst := strconv.ParseInt(from, 10, 64)
	last, errLast := strconv.ParseInt(to, 10, 64)
	size, errSize := strconv.ParseInt(total, 10, 64)
	return first, last, size, errFirst == nil && errLast == nil && errSize == nil && first >= 0 && last >= first && last < size
}

type retryWindows struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (w *retryWindows) record(origin, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	now := time.Now()
	deadline, err := http.ParseTime(value)
	if err != nil {
		seconds, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) {
			return
		}
		deadline = now.Add(time.Duration(seconds) * time.Second)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if deadline.After(now) && deadline.After(w.until[origin]) {
		w.until[origin] = deadline
	}
}

func (w *retryWindows) wait(ctx context.Context, origin string) error {
	for {
		delay := w.delay(origin)
		if delay <= 0 {
			return ctx.Err()
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (w *retryWindows) delay(origin string) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Until(w.until[origin])
}

type rateLimit struct {
	bytesPerSecond int64
	mu             sync.Mutex
	next           time.Time
}

func (r *rateLimit) wait(ctx context.Context, n int) error {
	if r.bytesPerSecond == 0 || n == 0 {
		return nil
	}
	r.mu.Lock()
	now := time.Now()
	if now.After(r.next) {
		r.next = now
	}
	r.next = r.next.Add(time.Duration(n) * time.Second / time.Duration(r.bytesPerSecond))
	delay := r.next.Sub(now)
	r.mu.Unlock()
	return wait(ctx, delay)
}

type responseReader struct {
	ctx   context.Context
	body  io.Reader
	idle  *time.Timer
	limit *rateLimit
	bytes *atomic.Int64
}

func (r responseReader) Read(buffer []byte) (int, error) {
	if r.limit.bytesPerSecond > 0 {
		// Keep throttled reads small to bound each worker's reservation of the shared limit.
		buffer = buffer[:min(int64(len(buffer)), 32<<10, max(1, r.limit.bytesPerSecond/10))]
	}
	r.idle.Reset(45 * time.Second)
	n, err := r.body.Read(buffer)
	r.idle.Stop()
	if err := r.limit.wait(r.ctx, n); err != nil {
		return 0, err
	}
	r.bytes.Add(int64(n))
	return n, err
}

// These files only hold a copy assembled from the resumable pieces.
func clearAbandonedAssemblies(ctx context.Context, root *os.Root) error {
	stage, err := root.Open(stageDir)
	if err != nil {
		return err
	}
	defer stage.Close()
	var stale []string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := stage.ReadDir(128)
		for _, e := range entries {
			digest, owned := strings.CutSuffix(e.Name(), ".part.assembling")
			if owned && validHex(digest, 32) {
				stale = append(stale, e.Name())
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	for _, name := range stale {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Remove(stageDir + "/" + name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func regularSize(root *os.Root, name string) (int64, error) {
	info, err := regularFile(root, name)
	if info == nil {
		return -1, err
	}
	return info.Size(), nil
}

func regularFile(root *os.Root, name string) (os.FileInfo, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return info, nil
}

func verify(ctx context.Context, root *os.Root, name string, resource resource) (bool, error) {
	size, err := regularSize(root, name)
	if err != nil || size != resource.Size {
		return false, err
	}
	return verifyDigest(ctx, root, name, resource)
}

func verifyDigest(ctx context.Context, root *os.Root, name string, resource resource) (bool, error) {
	digest, err := hashFile(ctx, root, name, resource.newHash())
	return digest == resource.digest(), err
}

var crc64XZ = crc64.MakeTable(crc64.ECMA)

func (f resource) newHash() hash.Hash {
	switch {
	case f.SHA256 != "":
		return sha256.New()
	case f.MD5 != "":
		return md5.New()
	default:
		return crc64.New(crc64XZ)
	}
}

func (f resource) matchesHash(h hash.Hash) bool {
	return fmt.Sprintf("%x", h.Sum(nil)) == f.digest()
}

func (f resource) digest() string {
	return cmp.Or(f.SHA256, f.MD5, f.CRC64)
}

func sha256File(ctx context.Context, root *os.Root, name string) (string, error) {
	return hashFile(ctx, root, name, sha256.New())
}

func hashFile(ctx context.Context, root *os.Root, name string, h hash.Hash) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = copyBuffered(h, contextReader{ctx, f})
	return fmt.Sprintf("%x", h.Sum(nil)), err
}

const copyBufferSize = 128 << 10

var copyBuffers = sync.Pool{New: func() any { return new([copyBufferSize]byte) }}

func copyBuffered(dst io.Writer, src io.Reader) (int64, error) {
	buffer := copyBuffers.Get().(*[copyBufferSize]byte)
	defer copyBuffers.Put(buffer)
	return io.CopyBuffer(dst, src, buffer[:])
}

type contextReader struct {
	context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
