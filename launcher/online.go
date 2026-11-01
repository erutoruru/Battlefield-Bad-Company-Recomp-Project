//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Online infrastructure: see ONLINE_API.md for the nobkonline.com API contract.

var httpClient = &http.Client{Timeout: 12 * time.Second}

func apiURL(path string) (string, error) {
	base := strings.TrimRight(settings.APIBase, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", errors.New("api_invalid")
	}
	host := u.Hostname()
	local := host == "localhost" || host == "127.0.0.1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return "", errors.New("api_https_required")
	}
	return base + path, nil
}

func apiCall(method, path string, body interface{}, token string, out interface{}) error {
	full, err := apiURL(path)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, full, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "BFBC-Launcher/"+launcherVersion)
	req.Header.Set("X-Title-Id", titleID)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return errors.New("network_error")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("http_%d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("bad_response")
		}
	}
	return nil
}

func evalJS(fn string, payload interface{}) {
	b, _ := json.Marshal(payload)
	view.Dispatch(func() { view.Eval("window." + fn + " && window." + fn + "(" + string(b) + ")") })
}

type loginResponse struct {
	Token       string `json:"token"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// loginAsync runs the request off the UI thread and reports via window.onLoginResult.
func loginAsync(username, password string) {
	go func() {
		var r loginResponse
		err := apiCall("POST", "/v1/auth/login", map[string]string{
			"username": username,
			"password": password,
		}, "", &r)
		if err == nil && r.Token == "" {
			err = errors.New("bad_response")
		}
		if err != nil {
			evalJS("onLoginResult", map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		enc, encErr := encryptToken(r.Token)
		if encErr != nil {
			evalJS("onLoginResult", map[string]interface{}{"ok": false, "error": "token_store_error"})
			return
		}
		name := r.Username
		if name == "" {
			name = username
		}
		settings.Username = name
		settings.TokenEnc = enc
		_ = settings.save()
		evalJS("onLoginResult", map[string]interface{}{"ok": true, "username": name, "display_name": r.DisplayName})
		go syncAchievements(r.Token)
	}()
}

func logout() {
	tok, _ := decryptToken(settings.TokenEnc)
	settings.TokenEnc = ""
	_ = settings.save()
	if tok != "" {
		go func() { _ = apiCall("POST", "/v1/auth/logout", nil, tok, nil) }()
	}
}

type serverStatus struct {
	Online  bool   `json:"online"`
	Players int    `json:"players"`
	Rooms   int    `json:"rooms"`
	Motd    string `json:"motd"`
}

func serverStatusAsync() {
	go func() {
		var s serverStatus
		if err := apiCall("GET", "/v1/status", nil, "", &s); err != nil {
			evalJS("onServerStatus", map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		evalJS("onServerStatus", map[string]interface{}{"ok": true, "status": s})
	}()
}

// syncAchievements pushes locally unlocked achievements to the server (best effort).
func syncAchievements(token string) {
	list, err := loadAchievements()
	if err != nil {
		return
	}
	type item struct {
		ID       uint32 `json:"id"`
		Filetime int64  `json:"filetime"`
	}
	var unlocked []item
	for _, a := range list.Items {
		if a.Unlocked {
			unlocked = append(unlocked, item{a.ID, a.Filetime})
		}
	}
	_ = apiCall("POST", "/v1/achievements/sync", map[string]interface{}{
		"title_id": titleID,
		"unlocked": unlocked,
	}, token, nil)
}
