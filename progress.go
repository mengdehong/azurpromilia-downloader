package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/pterm/pterm"
	"golang.org/x/term"
)

type fileProgress struct {
	saved         atomic.Int64
	received      atomic.Int64
	localFallback atomic.Bool
}

type downloadSlot struct {
	fileProgress
	job     *planFile
	started time.Time
}

type downloadStats struct {
	started, completed, failed             int
	saved, received, downloaded, remaining int64
}

// Totals include the planned cache for every job. Overlay changes as workers run or finish.
func (s *downloadStats) add(job *planFile, saved, received int64, localFallback bool) {
	delta := saved - job.Cached
	s.saved += delta
	s.received += received
	if job.Local == "" {
		transferred := job.File.networkRemaining(job.Cached) - job.File.networkRemaining(saved)
		s.downloaded += transferred
		s.remaining -= transferred
	} else if localFallback {
		remaining := job.File.networkRemaining(saved)
		s.downloaded += job.File.networkRemaining(0) - remaining
		s.remaining += remaining
	}
}

type downloadProgress struct {
	// Lock only when assigning, retiring, or sampling slots. Transfers update their own counters.
	mu           sync.Mutex
	slots        []downloadSlot
	totals       downloadStats
	firstFailure error
}

func newDownloadProgress(p plan, workers int) *downloadProgress {
	progress := &downloadProgress{slots: make([]downloadSlot, workers)}
	progress.totals.saved = p.ReuseBytes
	for _, job := range p.Jobs {
		progress.totals.saved += job.Cached
		if job.Local == "" {
			remaining := job.File.networkRemaining(job.Cached)
			progress.totals.downloaded += job.File.networkRemaining(0) - remaining
			progress.totals.remaining += remaining
		}
	}
	return progress
}

func (p *downloadProgress) start(slot *downloadSlot, job *planFile) {
	p.mu.Lock()
	defer p.mu.Unlock()
	slot.saved.Store(job.Cached)
	slot.received.Store(0)
	slot.localFallback.Store(false)
	slot.job, slot.started = job, time.Now()
	p.totals.started++
}

func (p *downloadProgress) finish(slot *downloadSlot, err, cancelErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	job := slot.job
	p.totals.add(job, min(slot.saved.Load(), job.File.Size), slot.received.Load(), slot.localFallback.Load())
	if err == nil {
		p.totals.completed++
	} else if !errors.Is(err, cancelErr) {
		p.totals.failed++
		if p.firstFailure == nil {
			p.firstFailure = fmt.Errorf("%s: %w", job.Path, err)
		}
	}
	slot.job = nil
}

func (p *downloadProgress) snapshot(active []activeDownload) downloadStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.totals
	for i := range p.slots {
		slot, task := &p.slots[i], &active[i]
		job := slot.job
		if job == nil {
			*task = activeDownload{}
			continue
		}
		if task.job != job {
			*task = activeDownload{job: job, rate: transferRate{at: slot.started}}
		}
		task.saved = min(slot.saved.Load(), job.File.Size)
		task.received = slot.received.Load()
		task.localFallback = slot.localFallback.Load()
		stats.add(job, task.saved, task.received, task.localFallback)
	}
	return stats
}

type transferRate struct {
	at    time.Time
	bytes int64
	speed float64
}

func (r *transferRate) sample(now time.Time, bytes int64) {
	elapsed := now.Sub(r.at).Seconds()
	if elapsed <= 0 {
		return
	}
	sample := float64(bytes-r.bytes) / elapsed
	if r.speed == 0 {
		r.speed = sample
	} else {
		r.speed = .25*sample + .75*r.speed
	}
	r.at, r.bytes = now, bytes
}

type activeDownload struct {
	job             *planFile
	saved, received int64
	localFallback   bool
	rate            transferRate
}

type downloadPanel struct {
	area  *pterm.AreaPrinter
	width int
}

func newDownloadPanel(out io.Writer) *downloadPanel {
	// PTerm v0.12.83's AreaPrinter draws directly to stdout.
	file, ok := out.(*os.File)
	if !ok || file != os.Stdout {
		return nil
	}
	if width, _ := terminalSize(out); width == 0 {
		return nil
	}
	return &downloadPanel{}
}

func (panel *downloadPanel) draw(out io.Writer, now time.Time, p plan, active []activeDownload, stats downloadStats, speed float64) bool {
	for i := range active {
		task := &active[i]
		if task.job != nil {
			task.rate.sample(now, task.received)
		}
	}
	width, height := terminalSize(out)
	if width <= 1 || height < len(active)+5 {
		panel.stop()
		return false
	}
	frame := downloadFrame(width-1, p, active, stats, speed)
	if frame == "" {
		panel.stop()
		return false
	}
	if panel.width != width {
		panel.stop()
	}
	if panel.area == nil {
		panel.area, _ = pterm.DefaultArea.WithRemoveWhenDone().Start(frame)
	} else if panel.area.GetContent() != frame {
		panel.area.Update(frame)
	}
	panel.width = width
	return true
}

func (panel *downloadPanel) stop() bool {
	if panel == nil || panel.area == nil {
		return false
	}
	_ = panel.area.Stop()
	if width, _ := terminalSize(os.Stdout); width > 1 && width < panel.width && os.Getenv("TMUX") != "" {
		// tmux may wrap old lines after a resize. AreaPrinter only clears the original rows.
		var extra int
		for line := range strings.SplitSeq(pterm.RemoveColorFromString(panel.area.GetContent()), "\n") {
			for runewidth.StringWidth(line) > width {
				line = line[len(runewidth.Truncate(line, width, "")):]
				extra++
			}
		}
		fmt.Fprint(os.Stdout, strings.Repeat("\x1b[1A\x1b[2K", extra))
	}
	panel.area = nil
	return true
}

func downloadFrame(width int, p plan, active []activeDownload, stats downloadStats, speed float64) string {
	const title = "Downloads"
	header := downloadHeader(width-len(title), p, stats, speed)
	if header == "" {
		return ""
	}
	compact := width < 55
	gap, etaLabel := "  ", "ETA "
	if compact {
		gap, etaLabel = " ", ""
	}
	type taskRow struct {
		name, size, speed, eta string
		fraction               float64
	}
	rows := make([]taskRow, 0, len(active))
	suffixCounts := make(map[string]int, len(active))
	for _, task := range active {
		if task.job == nil {
			continue
		}
		name := task.job.Path
		for {
			suffixCounts[name]++
			_, suffix, found := strings.Cut(name, "/")
			if !found {
				break
			}
			name = suffix
		}
	}
	var longestName, sizeWidth, speedWidth, etaWidth int
	for _, task := range active {
		job := task.job
		if job == nil {
			continue
		}
		size := job.File.Size
		done := task.saved
		var fraction float64
		if size > 0 {
			fraction = float64(done) / float64(size)
		}
		row := taskRow{
			name: job.Path, fraction: fraction, size: downloadSize(size),
			speed: humanBytes(int64(task.rate.speed)) + "/s",
			eta:   progressETA(size-done, task.rate.speed),
		}
		switch {
		case size > 0 && done == size:
			row.speed, row.eta = "Finishing", "—"
		case job.Local != "" && !task.localFallback:
			row.speed, row.eta = "Copying", "—"
		case task.rate.speed < 1:
			row.speed, row.eta = "Waiting", "—"
		}
		for {
			_, suffix, found := strings.Cut(row.name, "/")
			if !found || suffixCounts[suffix] > 1 {
				break
			}
			row.name = suffix
		}
		if compact {
			row.size = strings.ReplaceAll(row.size, " ", "")
			row.speed = strings.ReplaceAll(row.speed, " ", "")
		}
		longestName = max(longestName, runewidth.StringWidth(row.name))
		sizeWidth = max(sizeWidth, len(row.size))
		speedWidth = max(speedWidth, len(row.speed))
		etaWidth = max(etaWidth, runewidth.StringWidth(row.eta))
		rows = append(rows, row)
	}

	// Reserve the numeric fields first so every task keeps the same columns.
	available := width - (len("100%") + sizeWidth + speedWidth + etaWidth + len(etaLabel) + 4*len(gap))
	if available < 4 {
		return ""
	}
	nameWidth, barCells := min(available, max(4, min(24, longestName))), 0
	if !compact {
		cellWidth := max(runewidth.RuneWidth('█'), runewidth.RuneWidth('░'))
		preferredName := min(nameWidth, max(14, available/2))
		barOverhead := len("[]") + len(gap)
		if cells := min(32, (available-preferredName-barOverhead)/cellWidth); cells >= 4 {
			barCells = cells
			nameWidth = preferredName
		}
	}

	var frame strings.Builder
	color, reset := accent(), clearAccent()
	fmt.Fprintln(&frame, color+title+reset+header)
	rule := strings.Repeat("─", width/runewidth.RuneWidth('─'))
	fmt.Fprintln(&frame, rule)
	for _, row := range rows {
		fmt.Fprint(&frame, panelName(row.name, nameWidth), gap)
		if barCells > 0 {
			filled := int(row.fraction * float64(barCells))
			fmt.Fprintf(&frame, "[%s%s%s%s]%s",
				color, strings.Repeat("█", filled), reset, strings.Repeat("░", barCells-filled), gap)
		}
		fmt.Fprintf(&frame, "%3d%%%s%*s%s%*s%s%s%s\n",
			int(row.fraction*100), gap, sizeWidth, row.size, gap, speedWidth, row.speed, gap, etaLabel, row.eta)
	}
	frame.WriteString(strings.Repeat("\n", len(active)-len(rows)))
	fmt.Fprintln(&frame, rule)
	queued := fmt.Sprintf("%d queued", len(p.Jobs)-stats.started)
	if stats.failed > 0 {
		queued += fmt.Sprintf("  ·  %d failed", stats.failed)
	}
	fmt.Fprintln(&frame, runewidth.Truncate(queued, width, ""))
	return frame.String()
}

func downloadHeader(width int, p plan, stats downloadStats, speed float64) string {
	done, left := downloadSize(stats.downloaded), downloadSize(stats.remaining)
	eta := progressETA(stats.remaining, speed)
	if stats.remaining == 0 {
		eta = "0s"
	}
	compactDone, compactLeft := strings.ReplaceAll(done, " ", ""), strings.ReplaceAll(left, " ", "")
	rate := humanBytes(int64(speed)) + "/s"
	completed := stats.completed + p.ReuseFiles
	for _, header := range []string{
		fmt.Sprintf("  %d/%d  ·  %s downloaded  ·  %s left  ·  ETA %s  ·  %s",
			completed, p.TotalFiles, done, left, eta, rate),
		fmt.Sprintf("  %d/%d · %s down · %s left · ETA %s · %s",
			completed, p.TotalFiles, done, left, eta, rate),
		fmt.Sprintf("  %d/%d  ·  %s down  ·  %s left  ·  ETA %s",
			completed, p.TotalFiles, done, left, eta),
		fmt.Sprintf(" ↓%s %s left ETA %s", compactDone, compactLeft, eta),
		fmt.Sprintf(" ↓%s +%s ETA %s", compactDone, compactLeft, eta),
		fmt.Sprintf(" ↓%s +%s %s", compactDone, compactLeft, eta),
	} {
		if runewidth.StringWidth(header) <= width {
			return header
		}
	}
	return ""
}

func panelName(name string, width int) string {
	// Keep PTerm's color markup out of filenames.
	name = strings.ReplaceAll(name, "<", "‹")
	const ellipsis = "…"
	cells := runewidth.StringWidth(name)
	if cells > width {
		budget := width - runewidth.StringWidth(ellipsis)
		tail := budget / 2
		if ext := runewidth.StringWidth(path.Ext(name)); ext > tail && ext < budget {
			tail = ext
		}
		head := runewidth.Truncate(name, budget-tail, "")
		end := strings.TrimLeft(runewidth.TruncateLeft(name, cells-tail, ""), " ")
		name = head + ellipsis + end
	}
	return runewidth.FillRight(runewidth.Truncate(name, width, ""), width)
}

func downloadSize(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2f GB", float64(n)/1_000_000_000)
	case n > 0 && n < 10_000:
		return "<0.01 MB"
	default:
		return fmt.Sprintf("%.2f MB", float64(n)/1_000_000)
	}
}

func terminalSize(out io.Writer) (int, int) {
	if file, ok := out.(*os.File); ok && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(file.Fd())) {
		if width, height, err := term.GetSize(int(file.Fd())); err == nil && width > 0 {
			return width, height
		}
		return 80, 0
	}
	return 0, 0
}

func accent() string {
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		return ""
	}
	return "\x1b[1;36m"
}

func clearAccent() string {
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		return ""
	}
	return "\x1b[0m"
}

func progressETA(left int64, speed float64) string {
	if speed <= 0 {
		return "—"
	}
	seconds := math.Ceil(float64(left) / speed)
	if seconds >= 365*24*60*60 {
		return "—"
	}
	return (time.Duration(seconds) * time.Second).String()
}

func showProgress(out io.Writer, files, totalFiles int, done, total int64, speed float64) {
	done = min(done, total)
	var fraction float64
	if total > 0 {
		fraction = float64(done) / float64(total)
	} else if totalFiles > 0 {
		fraction = float64(files) / float64(totalFiles)
	}
	bars := int(fraction * 20)
	eta := progressETA(total-done, speed)
	line := fmt.Sprintf("%s%s %5.1f%%  %s / %s  %s/s  ETA %s  ·  %d/%d files",
		progressBars[:bars*len("━")], progressTrack[bars*len("─"):], fraction*100,
		downloadSize(done), downloadSize(total), humanBytes(int64(speed)), eta, files, totalFiles)
	width, _ := terminalSize(out)
	if width == 0 {
		fmt.Fprintln(out, line)
		return
	}
	if runewidth.StringWidth(line) >= width {
		line = fmt.Sprintf("%5.1f%%   %s/s   ETA %s   %d/%d", fraction*100, humanBytes(int64(speed)), eta, files, totalFiles)
	}
	if runewidth.StringWidth(line) >= width {
		line = fmt.Sprintf("%.1f%%", fraction*100)
		rate := strings.ReplaceAll(humanBytes(int64(speed)), " ", "") + "/s"
		if len(line)+1+len(rate) < width {
			line += " " + rate
		}
	}
	fmt.Fprint(out, "\r\x1b[2K", accent(), runewidth.Truncate(line, width-1, ""), clearAccent())
}

const progressBars = "━━━━━━━━━━━━━━━━━━━━"
const progressTrack = "────────────────────"

func clearProgress(out io.Writer) {
	if width, _ := terminalSize(out); width > 0 {
		fmt.Fprint(out, "\r\x1b[2K")
	}
}
