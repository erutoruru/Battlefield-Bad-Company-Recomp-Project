//go:build windows

// Battlefield: Bad Company - PC Recompilation Launcher
package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	webview2 "github.com/jchv/go-webview2"
)

const launcherVersion = "1.3.9"
const titleID = "454107F9"

//go:embed ui.html
var uiHTML string

//go:embed assets/background.jpg
var defaultBackground []byte

//go:embed assets/icon.png
var logoPNG []byte

var (
	launcherDir string
	settings    *Settings
	view        webview2.WebView
)

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

type Settings struct {
	Lang           string            `json:"lang"`
	GameData       string            `json:"game_data"`
	CloseOnLaunch  bool              `json:"close_on_launch"`
	ConnectionMode string            `json:"connection_mode"` // offline | online
	APIBase        string            `json:"api_base"`
	Username       string            `json:"username"`
	TokenEnc       string            `json:"token_enc"`
	RoomName       string            `json:"room_name"`
	GPUPreference  int               `json:"gpu_preference"` // 0 auto, 1 power saving, 2 high performance
	SkipIntros     bool              `json:"skip_intros"`
	AutoAim        bool              `json:"auto_aim"`
	MotionBlur     bool              `json:"motion_blur"`
	FilmGrain      bool              `json:"film_grain"`
	FullResFx      *bool             `json:"full_res_fx,omitempty"` // nil = default (on)
	SprintZoom     bool              `json:"sprint_zoom"`
	HdrCompat      *bool             `json:"hdr_compat,omitempty"` // nil = default (on)
	ButtonPrompts  string            `json:"button_prompts"`       // xbox | ps | kbm
	ShadowDistance string            `json:"shadow_distance"`      // default | far | ultra
	FovH           int               `json:"fov_h"`                // horizontal FOV at 16:9, 0 = game default
	WeaponSize     int               `json:"weapon_size"`          // percent, 0/100 = game default
	TankSpeed      int               `json:"tank_speed"`           // heavy turret traverse, percent, 0/100 = game default
	Cvars          map[string]string `json:"cvars"`
}

// Always written so the game works correctly; not user-editable.
var fixedCvars = map[string]string{
	"gpu_plugin":                        "xenos",
	"render_target_path_d3d12":          "rtv", // ROV is unplayably slow for this game
	"allow_game_relative_writes":        "true",
	"execute_unclipped_draw_vs_on_cpu":  "true",
	"gpu_allow_invalid_fetch_constants": "true",
	"license_mask":                      "4294967295",
}

func defaultCvars() map[string]string {
	return map[string]string{
		// Graphics
		"resolution_scale":         "1",
		"fullscreen":               "true",
		"fullscreen_exclusive":     "false",
		"window_width":             "1280",
		"window_height":            "720",
		"monitor":                  "0",
		"vsync":                    "true",
		"present_letterbox":        "true",
		"present_dither":           "false",
		"native_2x_msaa":           "true",
		"anisotropic_override":     "3",
		"readback_resolve":         "fast",
		"store_shaders":            "true",
		"async_shader_compilation": "true",
		// Audio
		"audio_maxqframes": "16",
		"audio_mute":       "false",
		// Input
		"input_backend":       "sdl",
		"mnk_mode":            "false",
		"mnk_mouse":           "true",
		"mnk_sensitivity":     "1.0",
		"bfbc_vehicle_sens":   "1.0",
		"bfbc_zoom_sens":      "1.0",
		"bfbc_weapon_keys":    "true",
		"bfbc_mouse_invert_y": "false",
		// Keyboard/mouse: FPS layout for Bad Company
		"keybind_right_trigger":  "LMB",
		"keybind_left_trigger":   "RMB",
		"keybind_a":              "Space",
		"keybind_b":              "E",
		"keybind_x":              "R",
		"keybind_y":              "V",
		"keybind_left_shoulder":  "Q",
		"keybind_right_shoulder": "G",
		"keybind_lstick_up":      "W",
		"keybind_lstick_down":    "S",
		"keybind_lstick_left":    "A",
		"keybind_lstick_right":   "D",
		"keybind_lstick_press":   "Shift",
		"keybind_rstick_press":   "Control",
		"keybind_rstick_up":      "Up",
		"keybind_rstick_down":    "Down",
		"keybind_rstick_left":    "Left",
		"keybind_rstick_right":   "Right",
		"keybind_dpad_up":        "1",
		"keybind_dpad_down":      "2",
		"keybind_dpad_left":      "3",
		"keybind_dpad_right":     "4",
		"keybind_back":           "Tab",
		"keybind_start":          "Escape",
		// Diagnostics
		"log_level": "info",
		// Game
		"user_language": "1",
	}
}

func defaultSettings() *Settings {
	return &Settings{
		Lang:           "en",
		GameData:       filepath.Join(launcherDir, "assets"),
		CloseOnLaunch:  true,
		ConnectionMode: "offline",
		APIBase:        "https://nobkonline.com/api",
		RoomName:       "Bad Company room",
		AutoAim:        true,
		MotionBlur:     true,
		FilmGrain:      true,
		ButtonPrompts:  "xbox",
		Cvars:          defaultCvars(),
	}
}

func settingsPath() string { return filepath.Join(launcherDir, "launcher.json") }

func loadSettings() *Settings {
	s := defaultSettings()
	b, err := os.ReadFile(settingsPath())
	if err == nil {
		_ = json.Unmarshal(b, s)
	}
	// Fill any cvars added in newer launcher versions.
	for k, v := range defaultCvars() {
		if _, ok := s.Cvars[k]; !ok {
			s.Cvars[k] = v
		}
	}
	return s
}

func (s *Settings) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath(), b, 0o644)
}

// ---------------------------------------------------------------------------
// Game paths & config
// ---------------------------------------------------------------------------

func gameExe() string {
	candidates := []string{
		filepath.Join(launcherDir, "bfbc.exe"),
		filepath.Join(launcherDir, "bin", "bfbc.exe"),
		filepath.Join(launcherDir, "out", "build", "win-amd64-release", "bfbc.exe"),
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
	}
	return candidates[len(candidates)-1]
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func tomlValue(v string) string {
	if v == "true" || v == "false" {
		return v
	}
	if _, err := strconv.ParseInt(v, 10, 64); err == nil {
		return v
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil && strings.Contains(v, ".") {
		return v
	}
	b, _ := json.Marshal(v) // JSON string escaping is valid TOML basic-string escaping here
	return string(b)
}

func writeGameConfig() error {
	exe := gameExe()
	cfg := filepath.Join(filepath.Dir(exe), "bfbc.toml")
	keys := make([]string, 0, len(settings.Cvars)+len(fixedCvars))
	all := map[string]string{}
	for k, v := range settings.Cvars {
		all[k] = v
	}
	for k, v := range fixedCvars {
		all[k] = v
	}
	// Direct mouse look (mouse_look.cpp): on whenever keyboard+mouse aiming is enabled.
	all["bfbc_mouse_look"] = fmt.Sprint(settings.Cvars["mnk_mode"] == "true" && settings.Cvars["mnk_mouse"] == "true")
	all["bfbc_mouse_sens"] = settings.Cvars["mnk_sensitivity"]
	applyModifierBinds(all)
	// Weapon slot keys: 1-5 are handled by the game build itself; free them from
	// any other binding (by default 1-4 are the D-pad) so they do not double-fire.
	if all["bfbc_weapon_keys"] == "true" && all["mnk_mode"] == "true" {
		for k, v := range all {
			if strings.HasPrefix(k, "keybind_") && len(v) == 1 && v[0] >= '1' && v[0] <= '5' {
				all[k] = ""
			}
		}
	}
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("# Bad Company Launcher v" + launcherVersion + " - generated file.\n")
	sb.WriteString("# Do not edit by hand; the launcher rewrites it on every game launch.\n\n")
	for _, k := range keys {
		sb.WriteString(fmt.Sprintf("%s = %s\n", k, tomlValue(all[k])))
	}
	// Before the one-time install the build folder does not exist yet; create it so
	// saving settings works and the config is already in place after the build.
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cfg, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	if abs, err := filepath.Abs(exe); err == nil {
		_ = applyGpuPreference(abs, settings.GPUPreference)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bound functions (called from JavaScript)
// ---------------------------------------------------------------------------

type State struct {
	Version        string            `json:"version"`
	Lang           string            `json:"lang"`
	GameData       string            `json:"game_data"`
	CloseOnLaunch  bool              `json:"close_on_launch"`
	Mode           string            `json:"connection_mode"`
	APIBase        string            `json:"api_base"`
	Username       string            `json:"username"`
	LoggedIn       bool              `json:"logged_in"`
	RoomName       string            `json:"room_name"`
	Cvars          map[string]string `json:"cvars"`
	ExePath        string            `json:"exe_path"`
	ExeFound       bool              `json:"exe_found"`
	GameValid      bool              `json:"game_valid"`
	Monitors       []string          `json:"monitors"`
	GPUs           []GPU             `json:"gpus"`
	GPUPreference  int               `json:"gpu_preference"`
	SkipIntros     bool              `json:"skip_intros"`
	AutoAim        bool              `json:"auto_aim"`
	MotionBlur     bool              `json:"motion_blur"`
	FilmGrain      bool              `json:"film_grain"`
	FullResFx      bool              `json:"full_res_fx"`
	SprintZoom     bool              `json:"sprint_zoom"`
	HdrCompat      bool              `json:"hdr_compat"`
	ButtonPrompts  string            `json:"button_prompts"`
	ShadowDistance string            `json:"shadow_distance"`
	FovH           int               `json:"fov_h"`
	WeaponSize     int               `json:"weapon_size"`
	TankSpeed      int               `json:"tank_speed"`
}

func getState() State {
	exe := gameExe()
	return State{
		Version:        launcherVersion,
		Lang:           settings.Lang,
		GameData:       settings.GameData,
		CloseOnLaunch:  settings.CloseOnLaunch,
		Mode:           settings.ConnectionMode,
		APIBase:        settings.APIBase,
		Username:       settings.Username,
		LoggedIn:       settings.TokenEnc != "",
		RoomName:       settings.RoomName,
		Cvars:          settings.Cvars,
		ExePath:        exe,
		ExeFound:       fileExists(exe),
		GameValid:      verifyGameData(settings.GameData).OK,
		Monitors:       listMonitors(),
		GPUs:           listGPUInfo(),
		GPUPreference:  settings.GPUPreference,
		SkipIntros:     settings.SkipIntros,
		AutoAim:        settings.AutoAim,
		MotionBlur:     settings.MotionBlur,
		FilmGrain:      settings.FilmGrain,
		FullResFx:      settings.fullResFx(),
		SprintZoom:     settings.SprintZoom,
		HdrCompat:      settings.hdrCompat(),
		ButtonPrompts:  normalizePromptMode(settings.ButtonPrompts),
		ShadowDistance: settings.ShadowDistance,
		FovH:           settings.FovH,
		WeaponSize:     settings.WeaponSize,
		TankSpeed:      settings.TankSpeed,
	}
}

type SavePayload struct {
	Lang           string            `json:"lang"`
	GameData       string            `json:"game_data"`
	CloseOnLaunch  bool              `json:"close_on_launch"`
	Mode           string            `json:"connection_mode"`
	APIBase        string            `json:"api_base"`
	RoomName       string            `json:"room_name"`
	GPUPreference  int               `json:"gpu_preference"`
	SkipIntros     bool              `json:"skip_intros"`
	AutoAim        bool              `json:"auto_aim"`
	MotionBlur     bool              `json:"motion_blur"`
	FilmGrain      bool              `json:"film_grain"`
	FullResFx      bool              `json:"full_res_fx"`
	SprintZoom     bool              `json:"sprint_zoom"`
	HdrCompat      bool              `json:"hdr_compat"`
	ButtonPrompts  string            `json:"button_prompts"`
	ShadowDistance string            `json:"shadow_distance"`
	FovH           int               `json:"fov_h"`
	WeaponSize     int               `json:"weapon_size"`
	TankSpeed      int               `json:"tank_speed"`
	Cvars          map[string]string `json:"cvars"`
}

func saveSettings(p SavePayload) error {
	if p.Lang != "" {
		settings.Lang = p.Lang
	}
	settings.GameData = p.GameData
	settings.CloseOnLaunch = p.CloseOnLaunch
	if p.Mode == "online" || p.Mode == "offline" {
		settings.ConnectionMode = p.Mode
	}
	if p.APIBase != "" {
		settings.APIBase = strings.TrimRight(p.APIBase, "/")
	}
	settings.RoomName = p.RoomName
	if p.GPUPreference >= 0 && p.GPUPreference <= 2 {
		settings.GPUPreference = p.GPUPreference
	}
	for k, v := range p.Cvars {
		if _, known := defaultCvars()[k]; known {
			settings.Cvars[k] = v
		}
	}
	settings.SkipIntros = false // intro skipping removed for now
	settings.AutoAim = p.AutoAim
	settings.MotionBlur = p.MotionBlur
	settings.FilmGrain = p.FilmGrain
	frf := p.FullResFx
	settings.FullResFx = &frf
	settings.SprintZoom = p.SprintZoom
	hc := p.HdrCompat
	settings.HdrCompat = &hc
	// FOV 61 (game default: 55 x 0.67 = ~37 vertical) is stored as 0.
	if p.FovH >= 50 && p.FovH <= 130 && p.FovH != 61 {
		settings.FovH = p.FovH
	} else {
		settings.FovH = 0
	}
	if p.WeaponSize >= 40 && p.WeaponSize < 100 {
		settings.WeaponSize = p.WeaponSize
	} else {
		settings.WeaponSize = 0
	}
	if p.TankSpeed > 100 && p.TankSpeed <= 400 {
		settings.TankSpeed = p.TankSpeed
	} else {
		settings.TankSpeed = 0
	}
	switch p.ShadowDistance {
	case "default", "far", "ultra":
		settings.ShadowDistance = p.ShadowDistance
	}
	if p.ButtonPrompts != "" {
		settings.ButtonPrompts = normalizePromptMode(p.ButtonPrompts)
	}
	if err := settings.save(); err != nil {
		return err
	}
	if err := writeGameConfig(); err != nil {
		return err
	}
	_ = restorePatchedIntros()
	if err := writeGameSettingsCfg(); err != nil {
		return fmt.Errorf("game_cfg_failed")
	}
	startAssetJob()
	return nil
}

func restoreDefaults() State {
	keepUser, keepTok, keepData := settings.Username, settings.TokenEnc, settings.GameData
	settings = defaultSettings()
	settings.Username, settings.TokenEnc, settings.GameData = keepUser, keepTok, keepData
	_ = settings.save()
	return getState()
}

type VerifyResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Files   int    `json:"files"`
}

func verifyGameData(dir string) VerifyResult {
	if !dirExists(dir) {
		return VerifyResult{false, "folder_missing", 0}
	}
	if !fileExists(filepath.Join(dir, "default.xex")) {
		return VerifyResult{false, "xex_missing", 0}
	}
	if !dirExists(filepath.Join(dir, "dist")) && !dirExists(filepath.Join(dir, "Dist")) {
		return VerifyResult{false, "data_missing", 0}
	}
	n := 0
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return VerifyResult{true, "ok", n}
}

func play() error {
	exe := gameExe()
	if !fileExists(exe) {
		return fmt.Errorf("exe_missing")
	}
	if v := verifyGameData(settings.GameData); !v.OK {
		return fmt.Errorf("%s", v.Message)
	}
	if err := writeGameConfig(); err != nil {
		return err
	}
	_ = restorePatchedIntros()
	_ = writeGameSettingsCfg() // errors are reported on save
	logPath, err := newLogPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe,
		"--game_data_root="+settings.GameData,
		"--log_file="+logPath,
	)
	cmd.Dir = filepath.Dir(exe)
	env := os.Environ()
	// Online infrastructure: the game reads these environment variables.
	env = append(env,
		"NOBK_MODE="+settings.ConnectionMode,
		"NOBK_API="+settings.APIBase,
		"NOBK_USER="+settings.Username,
	)
	if settings.ConnectionMode == "online" {
		if tok, err := decryptToken(settings.TokenEnc); err == nil && tok != "" {
			env = append(env, "NOBK_TOKEN="+tok)
		}
	}
	cmd.Env = env
	closeAfter := settings.CloseOnLaunch
	job := snapshotAssetJob()
	go func() {
		// Patched game files must be in place before the game opens its archives.
		runAssetJob(job)
		if err := cmd.Start(); err != nil {
			evalJS("onPlayError", map[string]string{"error": err.Error()})
			return
		}
		_ = cmd.Process.Release()
		if closeAfter {
			view.Dispatch(func() { view.Terminate() })
		}
	}()
	return nil
}

// Work on game files (button prompts, FOV/weapon size) runs off the UI thread,
// one job at a time.
var assetMu sync.Mutex

type assetJob struct {
	gameData   string
	mode       string
	binds      map[string]string
	vfov, rfov float32
	linear     bool // linear vehicle aim (keyboard/mouse)
	slots      bool // weapon slot keys 1-5
	sprintZoom bool // keep the game's zoom while sprinting
	tankSpeed  int  // heavy turret traverse percent (0 = original)
}

func snapshotAssetJob() assetJob {
	binds := map[string]string{}
	for k, v := range settings.Cvars {
		if strings.HasPrefix(k, "keybind_") {
			binds[k] = v
		}
	}
	vfov := float32(0) // 0 = game default
	if settings.FovH > 0 {
		vfov = float32(math.Round(horizontalToVertical(float64(settings.FovH))*100) / 100)
	}
	rfov := float32(defaultRenderFov)
	if settings.WeaponSize > 0 && settings.WeaponSize < 100 {
		rfov = float32(math.Round(weaponRenderFov(float64(settings.WeaponSize))*100) / 100)
	}
	linear := settings.Cvars["mnk_mode"] == "true" && settings.Cvars["mnk_mouse"] == "true"
	// Adding SelectWeapon2..5 to the soldier input map crashes the game (the engine
	// only has room for the console actions), so the map is always kept original;
	// any earlier patch is restored from the backup.
	slots := false
	return assetJob{settings.GameData, normalizePromptMode(settings.ButtonPrompts), binds, vfov, rfov, linear, slots,
		settings.SprintZoom, settings.TankSpeed}
}

func (j assetJob) upToDate() bool {
	return promptsUpToDate(j.gameData, j.mode, j.binds) && fovUpToDate(j.gameData, j.vfov, j.rfov, j.linear, j.slots, j.sprintZoom, j.tankSpeed)
}

func runAssetJob(j assetJob) {
	if j.upToDate() {
		return
	}
	assetMu.Lock()
	defer assetMu.Unlock()
	if !promptsUpToDate(j.gameData, j.mode, j.binds) {
		evalJS("onPrompts", map[string]string{"state": "busy"})
		if err := applyButtonPrompts(j.gameData, j.mode, j.binds); err != nil {
			evalJS("onPrompts", map[string]string{"state": "error", "error": err.Error()})
		} else {
			evalJS("onPrompts", map[string]string{"state": "done"})
		}
	}
	if !fovUpToDate(j.gameData, j.vfov, j.rfov, j.linear, j.slots, j.sprintZoom, j.tankSpeed) {
		err := applyFov(j.gameData, j.vfov, j.rfov, j.linear, j.slots, j.sprintZoom, j.tankSpeed, func(i, n int) {
			evalJS("onFovProgress", map[string]int{"i": i + 1, "n": n})
		})
		if err != nil {
			evalJS("onFovProgress", map[string]interface{}{"error": err.Error()})
		} else {
			evalJS("onFovProgress", map[string]bool{"done": true})
		}
	}
}

// startAssetJob applies changed game-file settings in the background.
func startAssetJob() {
	j := snapshotAssetJob()
	if j.upToDate() {
		return
	}
	go runAssetJob(j)
}

const maxLogFiles = 20

func logsDir() string { return filepath.Join(launcherDir, "logs") }

// newLogPath returns logs\log_YYYY-MM-DD_HH-MM-SS.txt and prunes old logs.
func newLogPath() (string, error) {
	dir := logsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	logs := listLogs()
	for len(logs) >= maxLogFiles {
		_ = os.Remove(logs[0])
		logs = logs[1:]
	}
	name := "log_" + time.Now().Format("2006-01-02_15-04-05") + ".txt"
	return filepath.Join(dir, name), nil
}

// listLogs returns log files sorted oldest first.
func listLogs() []string {
	m, _ := filepath.Glob(filepath.Join(logsDir(), "log_*.txt"))
	sort.Strings(m) // timestamped names sort chronologically
	return m
}

func openLogs() error {
	logs := listLogs()
	if len(logs) == 0 {
		return fmt.Errorf("log_missing")
	}
	return exec.Command("notepad.exe", logs[len(logs)-1]).Start()
}

func openFolder(which string) error {
	var p string
	switch which {
	case "game":
		p = settings.GameData
	case "logs":
		p = logsDir()
		_ = os.MkdirAll(p, 0o755)
	case "saves":
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, "Documents", "bfbc")
	default:
		p = launcherDir
	}
	return exec.Command("explorer.exe", p).Start()
}

func readBackground() string {
	dir := filepath.Join(launcherDir, "launcher_assets")
	for _, name := range []string{"background.jpg", "background.jpeg", "background.png", "background.webp"} {
		p := filepath.Join(dir, name)
		if b, err := os.ReadFile(p); err == nil {
			mime := "image/jpeg"
			switch strings.ToLower(filepath.Ext(name)) {
			case ".png":
				mime = "image/png"
			case ".webp":
				mime = "image/webp"
			}
			return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
		}
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(defaultBackground)
}

func openURL(u string) error {
	if !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("bad_url")
	}
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

func readLogo() string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(logoPNG)
}

// ---------------------------------------------------------------------------

func main() {
	runtime.LockOSThread()
	exe, _ := os.Executable()
	launcherDir = filepath.Dir(exe)
	settings = loadSettings()

	dataPath := filepath.Join(os.Getenv("LOCALAPPDATA"), "BFBCLauncher")
	view = webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  dataPath,
		WindowOptions: webview2.WindowOptions{
			Title:  "Battlefield: Bad Company — PC Recompilation",
			Width:  1040,
			Height: 740,
			IconId: 2, // rsrc: manifest=1, icon group=2
			Center: true,
		},
	})
	if view == nil {
		showError("WebView2 runtime not found.\n\nPlease install the Microsoft Edge WebView2 Runtime:\nhttps://go.microsoft.com/fwlink/p/?LinkId=2124703")
		return
	}
	defer view.Destroy()
	scale := dpiScale()
	view.SetSize(int(1040*scale), int(740*scale), webview2.HintFixed)

	must(view.Bind("goGetState", getState))
	must(view.Bind("goInstall", installGame))
	must(view.Bind("goSave", saveSettings))
	must(view.Bind("goRestoreDefaults", restoreDefaults))
	must(view.Bind("goVerify", verifyGameData))
	must(view.Bind("goBrowse", browseFolder))
	must(view.Bind("goPlay", play))
	must(view.Bind("goOpenLogs", openLogs))
	must(view.Bind("goOpenFolder", openFolder))
	must(view.Bind("goBackground", readBackground))
	must(view.Bind("goLogo", readLogo))
	must(view.Bind("goOpenURL", openURL))
	must(view.Bind("goAchievements", loadAchievements))
	must(view.Bind("goLogin", loginAsync))
	must(view.Bind("goLogout", logout))
	must(view.Bind("goServerStatus", serverStatusAsync))

	view.SetHtml(uiHTML)
	view.Run()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// XInput button masks for each keybind cvar (0x10000 = LT, 0x20000 = RT).
var keybindMask = map[string]uint32{
	"keybind_dpad_up": 0x0001, "keybind_dpad_down": 0x0002, "keybind_dpad_left": 0x0004,
	"keybind_dpad_right": 0x0008, "keybind_start": 0x0010, "keybind_back": 0x0020,
	"keybind_lstick_press": 0x0040, "keybind_rstick_press": 0x0080,
	"keybind_left_shoulder": 0x0100, "keybind_right_shoulder": 0x0200,
	"keybind_a": 0x1000, "keybind_b": 0x2000, "keybind_x": 0x4000, "keybind_y": 0x8000,
	"keybind_left_trigger": 0x10000, "keybind_right_trigger": 0x20000,
}

// applyModifierBinds moves Shift/Control/Alt bindings out of ReXGlue's keyboard
// emulation (where a lone modifier never triggers and blocks all other keys while
// held) into bfbc_mod_* cvars handled by mouse_look.cpp.
func applyModifierBinds(all map[string]string) {
	mods := map[string]uint32{"Shift": 0, "Control": 0, "Alt": 0}
	for cv, mask := range keybindMask {
		v := strings.TrimSpace(all[cv])
		if _, isMod := mods[v]; isMod {
			mods[v] |= mask
			all[cv] = ""
		}
	}
	all["bfbc_mod_shift"] = fmt.Sprint(mods["Shift"])
	all["bfbc_mod_ctrl"] = fmt.Sprint(mods["Control"])
	all["bfbc_mod_alt"] = fmt.Sprint(mods["Alt"])
}

// fullResFx: draw particles (smoke, dust) at full resolution. The game's half
// resolution particle pass composites with a blue tint under emulation.
func (s *Settings) fullResFx() bool { return s.FullResFx == nil || *s.FullResFx }

// hdrCompat: more headroom for the Xbox 360 7e3 HDR format (see gamecfg.go);
// explosions and smoke otherwise overflow (white flashes, blue tint).
func (s *Settings) hdrCompat() bool { return s.HdrCompat == nil || *s.HdrCompat }

// installGame starts the one-time build (Install.cmd -> tools/install.ps1) in its own
// console window: it installs the toolchain if needed, downloads the ReXGlue SDK,
// generates the recompiled code from the user's own default.xex and builds bfbc.exe.
func installGame() string {
	script := filepath.Join(launcherDir, "Install.cmd")
	if !fileExists(script) {
		return "Install.cmd not found: " + script
	}
	cmd := exec.Command("cmd", "/c", "start", "BFBC Install", "/D", launcherDir, script)
	if err := cmd.Start(); err != nil {
		return err.Error()
	}
	return ""
}
