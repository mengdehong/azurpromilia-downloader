package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	jpIndexBase = "https://raw.githubusercontent.com/frostlinuxdev/nadare-index/main/yostar/"
	jpIndexKey  = "azurpromilia-jp-yostar"
	jpGameTag   = "AZUPRO_JP"
	jpCDNHost   = "launcher-pkg-azupro-jp.yo-star.com"
)

type jpRelease struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

func queryJP(ctx context.Context, client *http.Client) (jpRelease, error) {
	data, err := fetchJPMetadata(ctx, client, "index.json", 1<<20)
	if err != nil {
		return jpRelease{}, err
	}
	var index map[string]jpRelease
	if err := json.Unmarshal(data, &index); err != nil {
		return jpRelease{}, fmt.Errorf("invalid JP version index: %w", err)
	}
	release := index[jpIndexKey]
	if !validBuildID(release.Version) || !validHex(release.SHA256, sha256.Size) {
		return jpRelease{}, errors.New("the Nadare index has no valid JP release")
	}
	return release, nil
}

func latestJPManifest(ctx context.Context, client *http.Client) (manifest, error) {
	release, err := queryJP(ctx, client)
	if err != nil {
		return manifest{}, err
	}
	data, err := fetchJPMetadata(ctx, client, jpIndexKey+".json", maxIndexBytes)
	if err != nil {
		return manifest{}, err
	}
	if !strings.EqualFold(fmt.Sprintf("%x", sha256.Sum256(data)), release.SHA256) {
		return manifest{}, errors.New("the JP manifest checksum does not match the Nadare index")
	}
	var payload struct {
		GameTag string `json:"game_tag"`
		Version string `json:"version"`
		BaseURL string `json:"base_url"`
		Files   []struct {
			Path  string  `json:"path"`
			Size  int64   `json:"size"`
			CRC64 *uint64 `json:"crc64"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return manifest{}, fmt.Errorf("invalid JP manifest: %w", err)
	}
	if payload.GameTag != jpGameTag || payload.Version != release.Version {
		return manifest{}, errors.New("the JP manifest game or version does not match the Nadare index")
	}
	base, err := url.Parse(payload.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host != jpCDNHost || base.User != nil ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return manifest{}, errors.New("the JP manifest has an invalid Yostar CDN URL")
	}
	list := manifest{Build: release.Version, Files: make([]resource, 0, len(payload.Files))}
	prefix := strings.TrimRight(base.Path, "/") + "/"
	address := url.URL{Scheme: base.Scheme, Host: base.Host}
	for _, file := range payload.Files {
		if file.CRC64 == nil {
			return manifest{}, fmt.Errorf("missing CRC64 for JP file %q", file.Path)
		}
		address.Path = prefix + file.Path
		list.Files = append(list.Files, resource{
			Path: file.Path, Size: file.Size, URL: address.String(), CRC64: fmt.Sprintf("%016x", *file.CRC64),
		})
	}
	return list, list.validate()
}

func fetchJPMetadata(ctx context.Context, client *http.Client, name string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jpIndexBase+name, nil)
	if err != nil {
		return nil, err
	}
	metadata := *client
	metadata.Timeout = 45 * time.Second
	resp, err := metadata.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not fetch JP %s from Nadare: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Nadare returned HTTP %d for JP %s", resp.StatusCode, name)
	}
	return readBounded(resp.Body, limit)
}
