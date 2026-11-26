package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	nexonGameInfo     = "https://ngmapi.nexon.com/game-info/2887@c55d"
	nexonCDNHost      = "azurpromilia-pc.dn.nexoncdn.co.kr"
	maxNexonChunkSize = 4 << 20
)

type nexonChunk struct {
	url, sha1 string
	size      int64
}

type nexonFile struct {
	Objects        map[string]string `json:"objects"`
	ObjectSizes    map[string]int64  `json:"object_sizes"`
	Size           int64             `json:"uncompressed_size"`
	CompressedSize int64             `json:"compressed_size"`
	Hash           string            `json:"hash"`
}

func latestKRManifest(ctx context.Context, client *http.Client) (manifest, error) {
	metadata := *client
	metadata.Timeout = 45 * time.Second
	data, err := fetchNexonMetadata(ctx, &metadata, nexonGameInfo, 1<<20)
	if err != nil {
		return manifest{}, err
	}
	var release struct {
		GameID   string `json:"game_id"`
		Manifest string `json:"manifest_name"`
		CDN      string `json:"setup_file_url"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return manifest{}, fmt.Errorf("invalid Nexon game info: %w", err)
	}
	base, err := url.Parse(release.CDN)
	if err != nil || base.Scheme != "https" || base.Host != nexonCDNHost || base.User != nil ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || release.GameID != "ngm_azurpromilia" ||
		!validHex(release.Manifest, 16) {
		return manifest{}, errors.New("Nexon game info has no valid KR release")
	}
	data, err = fetchNexonMetadata(ctx, &metadata, base.JoinPath(release.Manifest).String(), maxIndexBytes)
	if err != nil {
		return manifest{}, err
	}
	return parseNexonManifest(data, release.Manifest, base)
}

func fetchNexonMetadata(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not fetch Nexon release metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Nexon returned HTTP %d for %s", resp.StatusCode, address)
	}
	return readBounded(resp.Body, limit)
}

func parseNexonManifest(data []byte, build string, base *url.URL) (manifest, error) {
	var payload struct {
		ChunkSize      int64                `json:"chunk_size"`
		Size           int64                `json:"total_uncompressed_size"`
		CompressedSize int64                `json:"total_compressed_size"`
		Files          map[string]nexonFile `json:"files"`
		EmptyFiles     []string             `json:"zero_files"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return manifest{}, fmt.Errorf("invalid Nexon manifest: %w", err)
	}
	if payload.ChunkSize < 1 || payload.ChunkSize > maxNexonChunkSize {
		return manifest{}, errors.New("invalid chunk size in the Nexon manifest")
	}
	list := manifest{Build: build, Files: make([]resource, 0, len(payload.Files)+len(payload.EmptyFiles))}
	var size, compressedSize int64
	for key, file := range payload.Files {
		f, err := file.resource(key, payload.ChunkSize, base)
		if err != nil {
			return manifest{}, err
		}
		if f.Size > math.MaxInt64-size || file.CompressedSize > math.MaxInt64-compressedSize {
			return manifest{}, errors.New("Nexon manifest sizes exceed the supported limit")
		}
		size += f.Size
		compressedSize += file.CompressedSize
		list.Files = append(list.Files, f)
	}
	for _, key := range payload.EmptyFiles {
		file := nexonFile{Hash: fmt.Sprintf("%x", sha1.Sum(nil))}
		f, err := file.resource(key, payload.ChunkSize, base)
		if err != nil {
			return manifest{}, err
		}
		list.Files = append(list.Files, f)
	}
	if size != payload.Size || compressedSize != payload.CompressedSize {
		return manifest{}, errors.New("file sizes do not match the Nexon manifest totals")
	}
	sort.Slice(list.Files, func(i, j int) bool { return list.Files[i].Path < list.Files[j].Path })
	return list, list.validate()
}

func (file nexonFile) resource(key string, chunkSize int64, base *url.URL) (resource, error) {
	encoded := base64.URLEncoding.Strict()
	name, err := encoded.DecodeString(key)
	if err != nil || encoded.EncodeToString(name) != key {
		return resource{}, fmt.Errorf("invalid filename in the Nexon manifest: %q", key)
	}
	f := resource{Path: strings.ReplaceAll(string(name), "\\", "/"), Size: file.Size,
		NexonSHA1: strings.ToLower(file.Hash), ChunkSize: chunkSize}
	count := int64(len(file.Objects))
	if file.Size < 0 || count != nexonChunkCount(file.Size, chunkSize) || len(file.Objects) != len(file.ObjectSizes) {
		return resource{}, fmt.Errorf("invalid chunk count for Nexon file %q", f.Path)
	}
	f.chunks = make([]nexonChunk, 0, count)
	combined := sha1.New()
	var compressed int64
	for i := int64(0); i < count; i++ {
		index := strconv.FormatInt(i, 10)
		digest, size := file.Objects[index], file.ObjectSizes[index]
		if !validHex(digest, sha1.Size) || size < 1 || size > chunkSize+(chunkSize>>4)+64 || size > math.MaxInt64-compressed {
			return resource{}, fmt.Errorf("invalid chunk %d for Nexon file %q", i, f.Path)
		}
		address := base.JoinPath(key + "." + index + "." + digest + ".nxgz").String()
		f.chunks = append(f.chunks, nexonChunk{url: address, sha1: strings.ToLower(digest), size: size})
		compressed += size
		_, _ = io.WriteString(combined, strings.ToLower(digest))
	}
	if compressed != file.CompressedSize || !f.matchesHash(combined) {
		return resource{}, fmt.Errorf("chunk metadata does not match Nexon file %q", f.Path)
	}
	return f, nil
}

func nexonChunkCount(size, chunkSize int64) int64 {
	if size == 0 {
		return 0
	}
	return (size-1)/chunkSize + 1
}

// Nexon hashes the concatenated lowercase SHA-1 strings for the uncompressed chunks.
type nexonHash struct {
	chunk     hash.Hash
	chunkSize int
	used      int
	completed []byte
}

func (h *nexonHash) Write(p []byte) (int, error) {
	size := len(p)
	for len(p) > 0 {
		n := min(len(p), h.chunkSize-h.used)
		_, _ = h.chunk.Write(p[:n])
		h.used += n
		p = p[n:]
		if h.used == h.chunkSize {
			h.completed = h.appendChunkHash(h.completed)
			h.chunk.Reset()
			h.used = 0
		}
	}
	return size, nil
}

func (h *nexonHash) Sum(prefix []byte) []byte {
	combined := sha1.New()
	_, _ = combined.Write(h.completed)
	if h.used > 0 {
		var encoded [2 * sha1.Size]byte
		_, _ = combined.Write(h.appendChunkHash(encoded[:0]))
	}
	return combined.Sum(prefix)
}

func (h *nexonHash) appendChunkHash(dst []byte) []byte {
	var sum [sha1.Size]byte
	return hex.AppendEncode(dst, h.chunk.Sum(sum[:0]))
}

func (h *nexonHash) Reset() {
	h.used = 0
	h.completed = h.completed[:0]
	h.chunk.Reset()
}

func (h *nexonHash) Size() int      { return sha1.Size }
func (h *nexonHash) BlockSize() int { return sha1.BlockSize }

func (d downloader) fetchNexonFile(ctx context.Context, f resource, part string, offset int64, progress *fileProgress) (retry bool, err error) {
	first := max(0, offset) / f.ChunkSize
	offset = first * f.ChunkSize
	progress.saved.Store(offset)
	file, err := d.root.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if err := file.Truncate(offset); err != nil {
		return false, err
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	for _, chunk := range f.chunks[first:] {
		data, retry, err := d.fetchNexonChunk(ctx, chunk, min(f.ChunkSize, f.Size-offset), progress)
		if err != nil {
			return retry, fmt.Errorf("chunk at byte %d: %w", offset, err)
		}
		if _, err := (progressWriter{file, &progress.saved}).Write(data); err != nil {
			return false, err
		}
		offset += int64(len(data))
	}
	if first > 0 {
		valid, err := verifyDigest(ctx, d.root, part, f)
		if err != nil {
			return false, err
		}
		if !valid {
			return true, errChecksum
		}
	}
	return false, nil
}

func (d downloader) fetchNexonChunk(ctx context.Context, chunk nexonChunk, size int64, progress *fileProgress) ([]byte, bool, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, chunk.url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	origin := transferOrigin(req.URL)
	if err := d.acquireTransfer(ctx, origin); err != nil {
		return nil, false, err
	}
	defer func() { <-d.transfers }()
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	if status := resp.StatusCode; status != http.StatusOK {
		if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			d.retries.record(origin, resp.Header.Get("Retry-After"))
		}
		return nil, status == 408 || status == 429 || status >= 500, fmt.Errorf("HTTP %d", status)
	}
	body := d.newResponseReader(reqCtx, resp.Body, cancel, &progress.received)
	defer body.idle.Stop()
	compressed, err := readBounded(body, chunk.size)
	if err != nil {
		return nil, true, err
	}
	if int64(len(compressed)) != chunk.size {
		return nil, true, fmt.Errorf("received %d compressed bytes, expected %d", len(compressed), chunk.size)
	}
	input := bytes.NewReader(compressed)
	reader, err := zlib.NewReader(input)
	if err != nil {
		return nil, true, err
	}
	defer reader.Close()
	data, err := readBounded(reader, size)
	if err != nil {
		return nil, true, err
	}
	if int64(len(data)) != size || input.Len() != 0 || fmt.Sprintf("%x", sha1.Sum(data)) != chunk.sha1 {
		return nil, true, errors.New("Nexon chunk verification failed")
	}
	return data, false, nil
}
