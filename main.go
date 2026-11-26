package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Args[1:], os.Stdout)
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "Cancelled.")
		os.Exit(130)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	command := "download"
	if len(args) > 0 {
		switch args[0] {
		case "download", "check", "plan", "verify":
			command, args = args[0], args[1:]
		case "help":
			args = []string{"--help"}
		}
	}
	name := filepath.Base(os.Args[0])
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() {
		if command == "check" {
			fmt.Fprintf(out, "Usage: %s check [options]\n", name)
			flags.PrintDefaults()
			return
		}
		fmt.Fprintln(out, "Azur Promilia Downloader")
		fmt.Fprintf(out, "  %-44s Download or update from the official servers\n", name+" [options]")
		fmt.Fprintf(out, "  %-44s Show the latest release and optionally find local installations\n", name+" check [options]")
		fmt.Fprintf(out, "  %-44s Show what would be downloaded\n", name+" plan [options]")
		fmt.Fprintf(out, "  %-44s Check game files against the latest version\n", name+" verify [options]")
		fmt.Fprintln(out)
		flags.PrintDefaults()
	}
	dir, dirHelp := ".", "Game resource directory"
	if command == "check" {
		dir, dirHelp = "", "Scan this directory and its subdirectories for game installations"
	}
	flags.StringVar(&dir, "dir", dir, dirHelp)
	regionFlag := flags.String("region", string(regionCN), "Game region (cn, jp, kr, tw)")
	options := downloadOptions{
		workers: defaultWorkers, verifyWorkers: defaultVerifyWorkers,
		retries: 3, parts: defaultParts, partSize: defaultPartSize, summary: true,
	}
	if command != "check" {
		flags.IntVar(&options.verifyWorkers, "verify-workers", options.verifyWorkers, "Number of files to verify at once")
	}
	if command == "download" || command == "plan" {
		flags.IntVar(&options.workers, "workers", options.workers, fmt.Sprintf("Concurrent file downloads (up to %d)", maxDownloadWorkers))
	}
	var rate string
	if command == "download" {
		flags.StringVar(&rate, "limit", "0", "Total download speed limit in MB/s (0 for unlimited, also accepts units such as MiB/s)")
	}
	var asJSON bool
	if command == "plan" {
		flags.BoolVar(&asJSON, "json", false, "Print the plan as JSON")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	region := gameRegion(*regionFlag)
	if !region.valid() {
		return fmt.Errorf("unsupported region %q (use cn, jp, kr, or tw)", region)
	}
	if options.workers < 1 {
		return errors.New("--workers must be at least 1")
	}
	options.workers = min(options.workers, maxDownloadWorkers)
	if options.verifyWorkers < 1 || options.verifyWorkers > maxVerifyWorkers {
		return fmt.Errorf("--verify-workers must be between 1 and %d", maxVerifyWorkers)
	}
	var target string
	var err error
	dirSet := false
	flags.Visit(func(f *flag.Flag) { dirSet = dirSet || f.Name == "dir" })
	if command != "check" || dirSet {
		if dir == "" {
			return errors.New("the game resource directory cannot be empty")
		}
		target, err = filepath.Abs(dir)
		if err != nil {
			return err
		}
	}
	if command == "download" {
		options.limit, err = parseRate(rate)
		if err != nil {
			return err
		}
	}

	client := transferClient()
	defer client.CloseIdleConnections()
	if command == "check" {
		return checkVersion(ctx, out, client, target, region)
	}
	loadManifest, versionSource := region.launcher().latestManifest, "official"
	switch region {
	case regionJP:
		loadManifest, versionSource = latestJPManifest, "JP"
	case regionKR:
		loadManifest, versionSource = latestKRManifest, "KR"
	case regionTW:
		versionSource = "TW"
	}
	if !asJSON {
		fmt.Fprintf(out, "Checking the latest %s version for %s\n", versionSource, target)
	}
	list, err := loadManifest(ctx, client)
	if err != nil {
		return err
	}
	if command != "download" {
		return inspectFiles(ctx, out, list, target, command, region, asJSON, options)
	}

	fmt.Fprintf(out, "Latest version %s\n", list.Build)
	if err := download(ctx, out, client, list, target, region, options); err != nil {
		return err
	}
	fmt.Fprintln(out, "Game files are ready.")
	return nil
}

func checkVersion(ctx context.Context, out io.Writer, client *http.Client, dir string, region gameRegion) error {
	var latest, launchEntry string
	switch region {
	case regionJP:
		release, err := queryJP(ctx, client)
		if err != nil {
			return err
		}
		latest = release.Version
		fmt.Fprintf(out, "JP version        %s (Nadare index)\n", latest)
	case regionKR:
		list, err := latestKRManifest(ctx, client)
		if err != nil {
			return err
		}
		latest = list.Build
		var downloadSize, installedSize int64
		for _, f := range list.Files {
			downloadSize += f.networkRemaining(0)
			installedSize += f.Size
		}
		fmt.Fprintf(out, "KR build          %s (Nexon manifest)\n", latest)
		fmt.Fprintf(out, "Download size     %s\n", humanBytes(downloadSize))
		fmt.Fprintf(out, "Install size      %s\n", humanBytes(installedSize))
	default:
		release, err := region.launcher().query(ctx, client, officialVersion)
		if err != nil {
			return err
		}
		latest, launchEntry = release.buildID(), release.LaunchEntry
		version := latest
		if release.Name != latest && validBuildID(release.Name) {
			version = fmt.Sprintf("%s (%s)", release.Name, latest)
		}
		label := "Official version"
		if region == regionTW {
			label = "TW version"
		}
		fmt.Fprintf(out, "%-18s%s\n", label, version)
		if release.PackageSize > 0 {
			fmt.Fprintf(out, "Download size     %s\n", humanBytes(release.PackageSize))
		}
		if release.InstallSize > 0 {
			fmt.Fprintf(out, "Install size      %s (official estimate)\n", humanBytes(release.InstallSize))
		}
	}
	if dir == "" {
		return nil
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	versions, err := scanInstalledVersions(ctx, root, launchEntry, region)
	if err != nil {
		return err
	}
	if len(versions) == 0 {
		fmt.Fprintf(out, "\nNo installed version found under %s.\n", dir)
		return nil
	}
	fmt.Fprintln(out, "\nInstalled versions")
	for _, folder := range slices.Sorted(maps.Keys(versions)) {
		local := versions[folder]
		status := "Update available"
		if local == latest {
			status = "Up to date"
		}
		fmt.Fprintf(out, "  %-10s  %-16s  %s\n", local, status, filepath.Join(dir, folder))
	}
	return nil
}

func transferClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxIdleConnsPerHost = maxConcurrentTransfers
	return &http.Client{Transport: transport}
}

func parseRate(input string) (int64, error) {
	value := strings.ToLower(strings.TrimSpace(input))
	value = strings.TrimSpace(strings.TrimSuffix(value, "/s"))
	scale := int64(1_000_000)
	for _, unit := range []struct {
		suffix string
		scale  int64
	}{
		{"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10},
		{"gb", 1_000_000_000}, {"mb", 1_000_000}, {"kb", 1_000}, {"b", 1},
	} {
		if strings.HasSuffix(value, unit.suffix) {
			value, scale = strings.TrimSuffix(value, unit.suffix), unit.scale
			break
		}
	}
	rate, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || rate < 0 || rate > math.MaxInt64/scale {
		return 0, fmt.Errorf("invalid speed limit %q (use a whole number in MB/s, e.g. 20, or specify a unit, e.g. 512KiB/s)", input)
	}
	return rate * scale, nil
}
