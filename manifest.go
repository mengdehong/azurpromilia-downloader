package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"path"
	"strings"
	"unicode"
)

type manifest struct {
	Build    string     `json:"build"`
	Files    []resource `json:"files"`
	Preserve []string   `json:"preserve,omitempty"`
	Bundle   *zipBundle `json:"zip,omitempty"`
}

type resource struct {
	Path    string   `json:"path"`
	URL     string   `json:"url,omitempty"`
	Size    int64    `json:"size"`
	SHA256  string   `json:"sha256,omitempty"`
	MD5     string   `json:"md5,omitempty"`
	Mirrors []string `json:"mirrors,omitempty"`
}

type zipBundle struct {
	Size int64  `json:"size"`
	MD5  string `json:"md5"`
}

func validPath(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return p != "." && fs.ValidPath(p) && !strings.ContainsAny(p, "\\:") && strings.IndexFunc(p, unicode.IsControl) == -1 && !strings.EqualFold(first, stageDir)
}

func validBuildID(build string) bool {
	return strings.TrimSpace(build) != "" && strings.IndexFunc(build, unicode.IsControl) < 0
}

func (list *manifest) validate() error {
	return list.validateFiles(true)
}

func (list *manifest) validateFiles(withURLs bool) error {
	if !validBuildID(list.Build) || len(list.Files) == 0 {
		return errors.New("the download list needs a build and at least one file")
	}
	for _, name := range list.Preserve {
		if !validPath(strings.TrimSuffix(name, "/")) {
			return fmt.Errorf("invalid protected path in the download list: %q", name)
		}
	}
	paths := make(map[string]struct{}, len(list.Files))
	var total int64
	for i := range list.Files {
		f := &list.Files[i]
		if !validPath(f.Path) {
			return fmt.Errorf("invalid path in the download list: %q", f.Path)
		}
		key := strings.ToLower(f.Path)
		if _, ok := paths[key]; ok {
			return fmt.Errorf("duplicate path in the download list: %q", f.Path)
		}
		paths[key] = struct{}{}
		if withURLs {
			for _, address := range append([]string{f.URL}, f.Mirrors...) {
				if !validHTTP(address) {
					return fmt.Errorf("invalid download URL for %s", f.Path)
				}
			}
		}
		if (!validHex(f.SHA256, 32) && f.SHA256 != "") || (!validHex(f.MD5, 16) && f.MD5 != "") ||
			(f.SHA256 == "" && f.MD5 == "") || f.Size < 0 || f.Size == math.MaxInt64 {
			return fmt.Errorf("invalid file size or checksum for %s", f.Path)
		}
		f.SHA256 = strings.ToLower(f.SHA256)
		f.MD5 = strings.ToLower(f.MD5)
		if f.Size > math.MaxInt64-total {
			return errors.New("total size exceeds the supported limit")
		}
		total += f.Size
	}
	if list.Bundle != nil {
		if list.Bundle.Size <= 0 || list.Bundle.Size != total || !validHex(list.Bundle.MD5, 16) {
			return errors.New("invalid archive size or checksum")
		}
		list.Bundle.MD5 = strings.ToLower(list.Bundle.MD5)
	}
	for file := range paths {
		for parent := path.Dir(file); parent != "."; parent = path.Dir(parent) {
			if _, ok := paths[parent]; ok {
				return fmt.Errorf("path listed as both a file and a directory: %q", parent)
			}
		}
	}
	return nil
}

func validHTTP(address string) bool {
	u, err := url.Parse(address)
	return err == nil && u.Hostname() != "" && u.User == nil && u.Fragment == "" &&
		(u.Scheme == "http" || u.Scheme == "https")
}

func validHex(input string, length int) bool {
	if len(input) != 2*length {
		return false
	}
	value, err := hex.DecodeString(input)
	return err == nil && len(value) == length
}
