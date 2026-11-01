//go:build windows

package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

type Achievement struct {
	ID          uint32 `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Unachieved  string `json:"unachieved"`
	Gamerscore  int    `json:"gamerscore"`
	Icon        string `json:"icon"`
	Unlocked    bool   `json:"unlocked"`
	Filetime    int64  `json:"filetime"`
	UnlockedAt  string `json:"unlocked_at"`
}

type AchievementList struct {
	Found          bool          `json:"found"`
	Items          []Achievement `json:"items"`
	Total          int           `json:"total"`
	UnlockedCount  int           `json:"unlocked_count"`
	Gamerscore     int           `json:"gamerscore"`
	GamerscoreMax  int           `json:"gamerscore_max"`
	MetadataFolder string        `json:"metadata_folder"`
}

func metadataDir() string {
	cands := []string{
		filepath.Join(launcherDir, "metadata"),
		filepath.Join(settings.GameData, "metadata"),
		filepath.Join(filepath.Dir(settings.GameData), "metadata"),
	}
	for _, c := range cands {
		if fileExists(filepath.Join(c, "achievements.toml")) {
			return c
		}
	}
	return ""
}

func documentsDir() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Documents")
}

// unquoteTOML decodes a TOML basic string (with surrounding quotes).
func unquoteTOML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	s = s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'u':
			if i+4 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 4
				}
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

var kvRe = regexp.MustCompile(`^([A-Za-z0-9_]+)\s*=\s*(.+?)\s*$`)

func parseMetadata(path string) ([]Achievement, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var list []Achievement
	var cur *Achievement
	var iconPath []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "[[achievements]]" {
			list = append(list, Achievement{})
			iconPath = append(iconPath, "")
			cur = &list[len(list)-1]
			continue
		}
		if cur == nil {
			continue
		}
		m := kvRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		k, v := m[1], m[2]
		switch k {
		case "id":
			n, _ := strconv.ParseUint(v, 10, 32)
			cur.ID = uint32(n)
		case "label":
			cur.Label = unquoteTOML(v)
		case "description":
			cur.Description = unquoteTOML(v)
		case "unachieved_description":
			cur.Unachieved = unquoteTOML(v)
		case "gamerscore":
			cur.Gamerscore, _ = strconv.Atoi(v)
		case "icon_path":
			iconPath[len(iconPath)-1] = unquoteTOML(v)
		}
	}
	dir := filepath.Dir(path)
	for i := range list {
		if iconPath[i] == "" {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(iconPath[i]))); err == nil {
			list[i].Icon = "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
		}
	}
	return list, sc.Err()
}

var unlockHeaderRe = regexp.MustCompile(`^\[unlocked\.(\d+)\]$`)

func parseUnlocks(path string) map[uint32]int64 {
	res := map[uint32]int64{}
	f, err := os.Open(path)
	if err != nil {
		return res
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var cur int64 = -1
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := unlockHeaderRe.FindStringSubmatch(line); m != nil {
			id, _ := strconv.ParseUint(m[1], 10, 32)
			cur = int64(id)
			res[uint32(id)] = 0
			continue
		}
		if cur >= 0 {
			if m := kvRe.FindStringSubmatch(line); m != nil && m[1] == "filetime" {
				ft, _ := strconv.ParseInt(m[2], 10, 64)
				res[uint32(cur)] = ft
			}
		}
	}
	return res
}

func filetimeToString(ft int64) string {
	if ft <= 0 {
		return ""
	}
	// FILETIME: 100ns intervals since 1601-01-01
	const epochDiff = 116444736000000000
	t := time.Unix(0, (ft-epochDiff)*100)
	return t.Local().Format("02.01.2006 15:04")
}

func loadAchievements() (AchievementList, error) {
	res := AchievementList{Items: []Achievement{}}
	dir := metadataDir()
	if dir == "" {
		return res, nil
	}
	items, err := parseMetadata(filepath.Join(dir, "achievements.toml"))
	if err != nil {
		return res, errors.New("metadata_error")
	}
	unlocks := parseUnlocks(filepath.Join(documentsDir(), "bfbc", "achievements", titleID+".toml"))
	for i := range items {
		if ft, ok := unlocks[items[i].ID]; ok {
			items[i].Unlocked = true
			items[i].Filetime = ft
			items[i].UnlockedAt = filetimeToString(ft)
			res.UnlockedCount++
			res.Gamerscore += items[i].Gamerscore
		}
		res.GamerscoreMax += items[i].Gamerscore
	}
	res.Found = true
	res.Items = items
	res.Total = len(items)
	res.MetadataFolder = dir
	return res, nil
}
