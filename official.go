package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Anonymous distribution settings from the official Windows launcher 1.0.1.1.
const (
	launcherVersion = "1.0.1.1"
	launcherID      = "93cdd43e096046138053253dc56d3562"
	launcherAppID   = "354"
	launcherChannel = "C000"
	launcherSalt    = "20bccb31-fb57-4683-b6d8-6314d43811f2"
	indexKey        = "mJ9dH2bR7fT1zW4eQ6cY8a=="
	indexIV         = "aP3sD8fG0hJ6kL9z"
)

const (
	officialVersion  = "/pc-launcher/client/gameversion/config"
	officialPackages = "/pc-launcher/client/gameversion/packageconfig"
)

var officialOrigins = []string{"https://api-grp.manjuu.com", "https://api-grp-l2.manjuu.com"}

type packageConfig struct {
	Code *int `json:"code"`
	Data struct {
		Released *gameRelease `json:"released"`
	} `json:"data"`
}

type gameRelease struct {
	Version     json.RawMessage `json:"versionCode"`
	Name        string          `json:"versionName"`
	PackageSize int64           `json:"packageSize"`
	InstallSize int64           `json:"packageInstallSize"`
	LaunchEntry string          `json:"launchEntry"`
	Domains     []string        `json:"domainList,omitempty"`
	PackageList []gamePackage   `json:"gamePackageList"`
	Excluded    []struct {
		Path string `json:"filePath"`
		Type int    `json:"fileType"`
	} `json:"gameExcludedFileList,omitempty"`
}

type gamePackage struct {
	Type int      `json:"packageType"`
	Path string   `json:"downloadUrl"`
	Size int64    `json:"fileSize"`
	MD5  string   `json:"fileMd5"`
	CDNs []string `json:"cdnBaseUrls,omitempty"`
}

func latestManifest(ctx context.Context, client *http.Client) (manifest, error) {
	metadata := *client
	metadata.Timeout = 45 * time.Second
	release, err := queryOfficial(ctx, &metadata, officialPackages)
	if err != nil {
		return manifest{}, err
	}
	pkg, err := release.selectPackage()
	if err != nil {
		return manifest{}, err
	}
	index, err := fetchPackageIndex(ctx, &metadata, pkg)
	if err != nil {
		return manifest{}, err
	}
	// Plain-text indexes are comma-separated. The encrypted form is Base64.
	if !bytes.ContainsRune(index, ',') {
		index, err = decryptIndex(index)
		if err != nil {
			return manifest{}, err
		}
	}
	var list manifest
	if pkg.Type == 1 {
		list, err = parseArchiveIndex(index, release.buildID(), pkg.CDNs)
	} else {
		list, err = parseDirectIndex(index, release.buildID(), pkg.CDNs)
	}
	if err != nil {
		return manifest{}, err
	}
	list.Preserve = release.preservedPaths()
	return list, list.validate()
}

// signParams renders the string that participates in the signature. Values are
// not URL encoded here; the launcher encodes the actual query separately with
// URLSearchParams, so signing and transport encoding stay independent.
func signParams(params url.Values) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var text strings.Builder
	for index, key := range keys {
		if index > 0 {
			text.WriteByte('&')
		}
		text.WriteString(key)
		text.WriteByte('=')
		text.WriteString(params.Get(key))
	}
	return text.String()
}

func queryOfficial(ctx context.Context, client *http.Client, endpoint string) (gameRelease, error) {
	query := url.Values{"launcherId": {launcherID}}
	if endpoint == officialPackages {
		query.Set("uid", "")
	}
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	digest := md5.Sum([]byte(timestamp + signParams(query) + launcherAppID + launcherChannel + launcherSalt))

	var ids [32]byte
	rand.Read(ids[:])
	trace := hex.EncodeToString(ids[16:])
	headers := map[string]string{
		"timestamp":                 timestamp,
		"sign":                      hex.EncodeToString(digest[:]),
		"device-id":                 hex.EncodeToString(ids[:16]),
		"grp-trace-id":              trace,
		"grp-app-id":                launcherAppID,
		"grp-platform":              "Launcher",
		"grp-channel-code":          launcherChannel,
		"grp-sub-channel-code":      "SC000",
		"sdk-version":               launcherVersion,
		"channel-sdk-version":       launcherVersion,
		"grp-launcher-version-code": launcherVersion,
		"lang":                      "zh-Hans",
		"grp-client-info":           fmt.Sprintf(`{"device_model":"Launcher","device_os":"Linux","app_version":%q,"grp_trace_id":%q,"spec_device_info":{}}`, launcherVersion, trace),
	}

	// Keep the signed request on the configured official origins.
	noRedirect := *client
	if noRedirect.Timeout == 0 {
		noRedirect.Timeout = 45 * time.Second
	}
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var failure error
	for _, origin := range officialOrigins {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+endpoint+"?"+query.Encode(), nil)
		if err != nil {
			return gameRelease{}, err
		}
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			failure = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			failure = fmt.Errorf("official API returned HTTP %d", resp.StatusCode)
			continue
		}
		data, err := readBounded(resp.Body, 4<<20)
		resp.Body.Close()
		if err != nil {
			failure = err
			continue
		}
		release, err := parseRelease(data, endpoint == officialPackages)
		if err == nil {
			return release, nil
		}
		failure = err
	}
	return gameRelease{}, fmt.Errorf("official version check failed: %w", failure)
}

func parseRelease(data []byte, needsPackage bool) (gameRelease, error) {
	var config packageConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return gameRelease{}, fmt.Errorf("could not read the official response: %w", err)
	}
	if config.Code == nil {
		return gameRelease{}, errors.New("the official response has no status code")
	}
	if *config.Code != 0 {
		return gameRelease{}, fmt.Errorf("official API rejected the request (code %d)", *config.Code)
	}
	release := config.Data.Released
	if release == nil {
		return gameRelease{}, errors.New("the official API returned no released version")
	}
	if needsPackage && len(release.PackageList) == 0 {
		return gameRelease{}, errors.New("the official API returned no released game package")
	}
	if !validBuildID(release.buildID()) {
		return gameRelease{}, errors.New("the official release has an invalid version")
	}
	return *release, nil
}

func (release gameRelease) preservedPaths() []string {
	var paths []string
	for _, excluded := range release.Excluded {
		name := strings.Trim(strings.ReplaceAll(excluded.Path, "\\", "/"), "/")
		if excluded.Type == 1 {
			name += "/"
		}
		paths = append(paths, name)
	}
	return paths
}

func (r gameRelease) buildID() string {
	var code string
	if err := json.Unmarshal(r.Version, &code); err == nil && code != "" {
		return code
	}
	var numeric json.Number
	if err := json.Unmarshal(r.Version, &numeric); err == nil && numeric != "" {
		return string(numeric)
	}
	return r.Name
}

func (r gameRelease) selectPackage() (gamePackage, error) {
	var invalid bool
	for _, kind := range []int{2, 1} {
		for _, pkg := range r.PackageList {
			if pkg.Type != kind {
				continue
			}
			if len(r.Domains) > 0 {
				pkg.CDNs = r.Domains
			}
			if pkg.Size < 1 || !validHex(pkg.MD5, 16) || len(resolveCDNs(pkg.Path, pkg.CDNs)) == 0 {
				invalid = true
				continue
			}
			return pkg, nil
		}
	}
	if invalid {
		return gamePackage{}, errors.New("the official package is missing its index URL, size or checksum")
	}
	return gamePackage{}, errors.New("the official build offers no supported package")
}

func resolveCDNs(remote string, bases []string) []string {
	remote = strings.ReplaceAll(remote, "\\", "/")
	if remote == "" {
		return nil
	}
	if validHTTP(remote) {
		return []string{remote}
	}
	ref, err := url.Parse(remote)
	if err != nil || ref.IsAbs() || ref.Host != "" {
		return nil
	}
	var addresses []string
	for _, address := range bases {
		if !validHTTP(address) {
			continue
		}
		base, err := url.Parse(strings.TrimRight(address, "/") + "/")
		if err == nil {
			resolved := base.ResolveReference(ref).String()
			if validHTTP(resolved) {
				addresses = append(addresses, resolved)
			}
		}
	}
	return addresses
}

func parseDirectIndex(data []byte, build string, bases []string) (manifest, error) {
	list := manifest{Build: build}
	first := true
	wantFiles, wantDirectories := -1, 0
	directories := 0
	err := readIndexRows(data, func(line int, fields []string) error {
		header := first && len(fields) == 3
		first = false
		if header {
			total, totalErr := strconv.Atoi(fields[0])
			files, filesErr := strconv.Atoi(fields[1])
			dirs, dirsErr := strconv.Atoi(fields[2])
			if totalErr != nil || filesErr != nil || dirsErr != nil || files < 0 || dirs < 0 || total < dirs || files != total-dirs {
				return fmt.Errorf("invalid index counts at line %d", line)
			}
			wantFiles, wantDirectories = files, dirs
			return nil
		}
		if len(fields) == 4 {
			directories++
			return nil
		}
		if len(fields) != 5 {
			return invalidIndexRow(line)
		}
		name := strings.ReplaceAll(fields[3], "\\", "/")
		f, err := indexResource(line, name, fields[1], fields[2], fields[4], bases)
		if err == nil {
			list.Files = append(list.Files, f)
		}
		return err
	})
	if err != nil {
		return list, err
	}
	if wantFiles >= 0 && (len(list.Files) != wantFiles || directories != wantDirectories) {
		return list, fmt.Errorf("index counts do not match: got %d files and %d directories, expected %d and %d",
			len(list.Files), directories, wantFiles, wantDirectories)
	}
	return list, nil
}

func parseArchiveIndex(data []byte, build string, bases []string) (manifest, error) {
	list := manifest{Build: build}
	err := readIndexRows(data, func(line int, fields []string) error {
		switch len(fields) {
		case 3:
			size, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || list.Bundle != nil {
				return invalidIndexRow(line)
			}
			list.Bundle = &zipBundle{Size: size, MD5: fields[2]}
			return nil
		case 5:
			name := fmt.Sprintf("slice-%06d", len(list.Files)+1)
			f, err := indexResource(line, name, fields[2], fields[3], fields[4], bases)
			if err == nil {
				list.Files = append(list.Files, f)
			}
			return err
		default:
			return invalidIndexRow(line)
		}
	})
	if err != nil {
		return list, err
	}
	if list.Bundle == nil {
		return list, errors.New("the archive index is missing the combined size and checksum")
	}
	return list, nil
}

func readIndexRows(data []byte, accept func(line int, fields []string) error) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 32<<10), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if text != "" {
			if err := accept(line, strings.Split(strings.TrimSuffix(text, ","), ",")); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func invalidIndexRow(line int) error {
	return fmt.Errorf("invalid official index at line %d", line)
}

func indexResource(line int, name, length, md5, remote string, bases []string) (resource, error) {
	size, err := strconv.ParseInt(length, 10, 64)
	urls := resolveCDNs(remote, bases)
	if err != nil || len(urls) == 0 {
		return resource{}, fmt.Errorf("invalid size or URL on index line %d", line)
	}
	return resource{Path: name, URL: urls[0], Mirrors: urls[1:], Size: size, MD5: md5}, nil
}

func fetchPackageIndex(ctx context.Context, client *http.Client, pkg gamePackage) ([]byte, error) {
	var failure error
	for _, address := range resolveCDNs(pkg.Path, pkg.CDNs) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			failure = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			failure = fmt.Errorf("index server returned HTTP %d", resp.StatusCode)
			continue
		}
		data, err := readBounded(resp.Body, 64<<20)
		resp.Body.Close()
		if err == nil {
			err = checkIndex(data, pkg)
		}
		if err == nil {
			return data, nil
		}
		failure = err
	}
	return nil, fmt.Errorf("could not download the official index: %w", failure)
}

func checkIndex(data []byte, pkg gamePackage) error {
	if int64(len(data)) != pkg.Size || !strings.EqualFold(fmt.Sprintf("%x", md5.Sum(data)), pkg.MD5) {
		return errors.New("the official index failed verification")
	}
	return nil
}

func decryptIndex(input []byte) ([]byte, error) {
	block, err := aes.NewCipher([]byte(indexKey))
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(input)))
	if err != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("invalid encrypted index")
	}
	cipher.NewCBCDecrypter(block, []byte(indexIV)).CryptBlocks(data, data)
	pad := int(data[len(data)-1])
	if pad < 1 || pad > aes.BlockSize {
		return nil, errors.New("could not decrypt the index")
	}
	for _, value := range data[len(data)-pad:] {
		if int(value) != pad {
			return nil, errors.New("could not decrypt the index")
		}
	}
	return data[:len(data)-pad], nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("input exceeds %s", humanBytes(limit))
	}
	return data, err
}
